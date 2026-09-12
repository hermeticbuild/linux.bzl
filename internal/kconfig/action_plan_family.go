package kconfig

// This file is the lossless handoff between independently evaluated image
// variants and their one symmetric execution graph. A snapshot retains
// planner-only facts which the v4 execution marker shards intentionally omit;
// the family reducer then assigns source and node identities from semantic
// content, never from variant order or Bazel storage paths.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

const (
	LinuxKernelActionPlanSnapshotSchema = "linux-kernel-action-plan-snapshot-v3"
	LinuxKernelFamilyPlanSchema         = "linux-kernel-family-plan-v7"
	LinuxKernelFamilyReuseReportSchema  = "linux-kernel-family-reuse-report-v2"

	// These paths are private allocations made by the Kbuild planner. Their
	// first path component below the prefix is an evaluation-local identity
	// which can include a config-specific profile digest. It is physical
	// placement, not part of the file's Kbuild-visible identity.
	familyIntermediateArtifactDirectory         = ".linux-bzl-intermediate"
	familySideOutputArtifactDirectory           = ".linux-bzl-side-outputs"
	familySideOutputResolutionArtifactDirectory = ".linux-bzl-side-output-resolution"
	familySelectionArtifactDirectory            = ".linux-bzl-versions"
	familyOwnedArtifactDirectory                = ".linux-bzl-family-artifacts"
	familyOwnedObservedStateDirectory           = ".linux-bzl-family-observed-side-output"

	// Snapshots contain source-derived recipes, but never arbitrary source-tree
	// bytes. This bound is deliberately generous for the canonical JSON of a
	// complete Linux plan and still prevents an accidental unbounded allocation
	// while decoding.
	MaxActionPlanSnapshotBytes = 512 << 20

	// Snapshot transport is deterministic gzip. Linux action plans are highly
	// repetitive JSON (the measured full-plan ratio is about 6.4:1), so a
	// separate 128 MiB compressed ceiling leaves substantial headroom while
	// bounding both remote downloads and compressed-input allocation. A valid
	// canonical snapshot must satisfy both independent limits.
	MaxActionPlanSnapshotCompressedBytes = 128 << 20
)

// LinuxKernelFamilyPlanSegments are the only independently expanded family
// plan shards. Prep and target intentionally share one segment: both run on
// the configured target execution platform, and keeping their exact edges in
// one marker tree avoids an artificial handoff inside that platform.
var LinuxKernelFamilyPlanSegments = map[string]bool{
	"prehost":   true,
	"bootstrap": true,
	"host":      true,
	"target":    true,
}

var linuxKernelFamilyPlanSegmentOrder = []string{"prehost", "bootstrap", "host", "target"}

func familyPlanSegmentForStage(stage string) (string, bool) {
	switch stage {
	case "prehost", "bootstrap", "host":
		return stage, true
	case "prep", "target":
		return "target", true
	default:
		return "", false
	}
}

// ActionPlanSnapshot is the in-memory form of canonical JSON owned by one final
// planner action. The file handoff is a deterministic gzip transport of those
// exact canonical bytes.
// ConfigDependencies and ConfigFiles are not executor metadata: they are the
// evidence needed to replace the old blanket config inputs with sound,
// content-addressed capsules before cross-variant identities are computed.
type ActionPlanSnapshot struct {
	Schema                    string                                        `json:"schema"`
	Toolsets                  map[string]string                             `json:"toolsets"`
	Sources                   []ActionPlanSource                            `json:"sources"`
	Recipes                   map[string]ActionRecipe                       `json:"recipes"`
	Nodes                     []ActionPlanNode                              `json:"nodes"`
	InputSets                 map[string]ActionPlanInputSetNode             `json:"input_sets"`
	Products                  []ActionPlanProduct                           `json:"products"`
	ConfigDependencies        map[string]ConfigDependencySet                `json:"config_dependencies"`
	ConfigFiles               map[string]string                             `json:"config_files"`
	InternalNodes             []string                                      `json:"internal_nodes,omitempty"`
	InternalOutputs           []ActionPlanSnapshotOutputRef                 `json:"internal_outputs,omitempty"`
	ProjectedGeneratorOutputs []ActionPlanSnapshotProjectedGeneratorOutputs `json:"projected_generator_outputs,omitempty"`
	ValidationRoots           []string                                      `json:"validation_roots,omitempty"`
}

type ActionPlanSnapshotOutputRef struct {
	NodeID string `json:"node_id"`
	Slot   int    `json:"slot"`
}

type ActionPlanSnapshotProjectedGeneratorOutputs struct {
	NodeID     string             `json:"node_id"`
	TargetSlot int                `json:"target_slot"`
	Outputs    []ActionPlanOutput `json:"outputs"`
}

// ActionPlanFamilyVariant is one named, independently evaluated member of an
// image family. Names are public repository path components and are therefore
// validated with the same strict component grammar as plan markers.
type ActionPlanFamilyVariant struct {
	Name     string
	Snapshot ActionPlanSnapshot
}

// ValidatedActionPlanFamilyVariant is an opaque family member read from a
// validated snapshot transport. Its snapshot is intentionally not exposed:
// callers cannot mutate the maps and slices after ReadActionPlanFamilyVariant
// validates them and before the one-shot family writer consumes them.
type ValidatedActionPlanFamilyVariant struct {
	name      string
	snapshot  ActionPlanSnapshot
	validated bool
}

// ReadActionPlanFamilyVariant validates a variant name and snapshot transport
// and seals both for one-shot family emission.
func ReadActionPlanFamilyVariant(name, filename string) (ValidatedActionPlanFamilyVariant, error) {
	if err := validatePlanName("family variant", name); err != nil {
		return ValidatedActionPlanFamilyVariant{}, err
	}
	snapshot, err := ReadActionPlanSnapshot(filename)
	if err != nil {
		return ValidatedActionPlanFamilyVariant{}, err
	}
	return ValidatedActionPlanFamilyVariant{name: name, snapshot: snapshot, validated: true}, nil
}

type ActionPlanFamilyView struct {
	Variant      string
	Tree         string
	NodeID       string
	Slot         int
	ArtifactPath string
}

type ActionPlanFamilyProduct struct {
	Variant string
	Name    string
	Tree    string
	Path    string
}

type ActionPlanFamilyValidation struct {
	Variant string
	NodeID  string
	Slot    int
}

// ActionPlanFamily is the one graph expanded by Bazel. Outputs retain their
// original logical tree and path metadata; Bazel derives their physical store
// leaf as nodes/<semantic node ID>/<slot>.
type ActionPlanFamily struct {
	Toolsets    map[string]string
	Sources     []ActionPlanSource
	Recipes     map[string]ActionRecipe
	Nodes       []ActionPlanNode
	InputSets   map[string]ActionPlanInputSetNode
	Products    []ActionPlanFamilyProduct
	Views       []ActionPlanFamilyView
	Validations []ActionPlanFamilyValidation
	Capsules    map[string]map[string]string
	Memberships map[string][]string
	Variants    []string

	// originalNodeIDs records the exact snapshot node which became each final
	// live family node. It is retained across localization/rekeying and filtered
	// by final reachability for verified execution-cut handoff, never inferred
	// from membership names.
	// Variant -> original snapshot node ID -> final semantic family node ID.
	originalNodeIDs map[string]map[string]string

	// Only the verified in-process replay writer supplies this immutable cut.
	// Its exact original bindings are additional liveness roots, not compiler
	// inputs or public views, so retaining prior executions cannot salt keys.
	executionCut *ActionPlanFamilyExecutionCut

	// Diagnostics are planner-local evidence for the reuse report. They are not
	// execution metadata and therefore never enter the family plan markers.
	opaqueReasons             map[string]map[string]int
	preciseCompileMemberships map[string][]string
	observedHeaderFrontier    *ActionPlanFamilyObservedHeaderFrontier
}

type ActionPlanFamilyReuseKind struct {
	Kind  string `json:"kind"`
	Nodes int    `json:"nodes"`
}

type ActionPlanFamilyReuseVariant struct {
	Name                               string                      `json:"name"`
	Nodes                              int                         `json:"nodes"`
	SharedNodes                        int                         `json:"shared_nodes"`
	ExclusiveNodes                     int                         `json:"exclusive_nodes"`
	TypedCompilerNodes                 int                         `json:"typed_compiler_nodes"`
	PreciseCompileNodes                int                         `json:"precise_compile_nodes"`
	PreciseCompilerCoverageBasisPoints int                         `json:"precise_compiler_coverage_basis_points"`
	Kinds                              []ActionPlanFamilyReuseKind `json:"kinds"`
}

type ActionPlanFamilyReuseGroup struct {
	Variants []string                    `json:"variants"`
	Nodes    int                         `json:"nodes"`
	Kinds    []ActionPlanFamilyReuseKind `json:"kinds"`
}

type ActionPlanFamilyReusePair struct {
	Left                                  string `json:"left"`
	Right                                 string `json:"right"`
	LeftNodes                             int    `json:"left_nodes"`
	RightNodes                            int    `json:"right_nodes"`
	SharedNodes                           int    `json:"shared_nodes"`
	LeftReuseBasisPoints                  int    `json:"left_reuse_basis_points"`
	RightReuseBasisPoints                 int    `json:"right_reuse_basis_points"`
	EligibleLeftNodes                     int    `json:"eligible_left_nodes"`
	EligibleRightNodes                    int    `json:"eligible_right_nodes"`
	EligibleSharedNodes                   int    `json:"eligible_shared_nodes"`
	EligibleLeftReuseBasisPoints          int    `json:"eligible_left_reuse_basis_points"`
	EligibleRightReuseBasisPoints         int    `json:"eligible_right_reuse_basis_points"`
	TypedLeftCompilerNodes                int    `json:"typed_left_compiler_nodes"`
	TypedRightCompilerNodes               int    `json:"typed_right_compiler_nodes"`
	TypedSharedCompilerNodes              int    `json:"typed_shared_compiler_nodes"`
	TypedLeftReuseBasisPoints             int    `json:"typed_left_reuse_basis_points"`
	TypedRightReuseBasisPoints            int    `json:"typed_right_reuse_basis_points"`
	PreciseLeftCompileNodes               int    `json:"precise_left_compile_nodes"`
	PreciseRightCompileNodes              int    `json:"precise_right_compile_nodes"`
	PreciseSharedCompileNodes             int    `json:"precise_shared_compile_nodes"`
	PreciseLeftReuseBasisPoints           int    `json:"precise_left_reuse_basis_points"`
	PreciseRightReuseBasisPoints          int    `json:"precise_right_reuse_basis_points"`
	EffectivePreciseLeftReuseBasisPoints  int    `json:"effective_precise_left_reuse_basis_points"`
	EffectivePreciseRightReuseBasisPoints int    `json:"effective_precise_right_reuse_basis_points"`
}

type ActionPlanFamilyOpaqueReason struct {
	Variant string `json:"variant"`
	Kind    string `json:"kind"`
	Reason  string `json:"reason"`
	Nodes   int    `json:"nodes"`
}

// ActionPlanFamilyReuseNode is the exact, independently checkable evidence
// behind every aggregate in the reuse report. One record exists for every
// semantic family node, in node-ID order. Memberships are execution
// memberships; precision is therefore only true when the compiler analysis is
// valid for every execution member of the shared node.
type ActionPlanFamilyReuseNode struct {
	NodeID         string   `json:"node_id"`
	Kind           string   `json:"kind"`
	Memberships    []string `json:"memberships"`
	TypedCompiler  bool     `json:"typed_compiler"`
	PreciseCompile bool     `json:"precise_compile"`
}

// ActionPlanFamilyPreciseCompileOutput identifies one physical output of a
// precise compiler node. Slot plus StorePath identifies the content-addressed
// executor artifact; ArtifactPath is its Kbuild-visible projection, and
// LogicalPath retains the planner's logical output identity.
type ActionPlanFamilyPreciseCompileOutput struct {
	Slot         int    `json:"slot"`
	Tree         string `json:"tree"`
	LogicalPath  string `json:"logical_path"`
	ArtifactPath string `json:"artifact_path"`
	StorePath    string `json:"store_path"`
}

// ActionPlanFamilyPreciseCompile records the final semantic identity of one
// precise compiler node. Outputs is the legacy compatibility spelling;
// OutputDetails is the unambiguous physical artifact identity for new
// consumers.
type ActionPlanFamilyPreciseCompile struct {
	NodeID        string                                 `json:"node_id"`
	Outputs       []string                               `json:"outputs"`
	OutputDetails []ActionPlanFamilyPreciseCompileOutput `json:"output_details"`
	Memberships   []string                               `json:"memberships"`
}

type ActionPlanFamilyReuseReport struct {
	Schema                 string                                  `json:"schema"`
	Toolsets               map[string]string                       `json:"toolsets"`
	Nodes                  []ActionPlanFamilyReuseNode             `json:"nodes"`
	Variants               []ActionPlanFamilyReuseVariant          `json:"variants"`
	MembershipGroups       []ActionPlanFamilyReuseGroup            `json:"membership_groups"`
	Pairs                  []ActionPlanFamilyReusePair             `json:"pairs"`
	VariantInstances       int                                     `json:"variant_instances"`
	UniqueNodes            int                                     `json:"unique_nodes"`
	SharedNodes            int                                     `json:"shared_nodes"`
	ReusedInstances        int                                     `json:"reused_instances"`
	OpaqueReasons          []ActionPlanFamilyOpaqueReason          `json:"opaque_reasons"`
	PreciseCompiles        []ActionPlanFamilyPreciseCompile        `json:"precise_compiles"`
	ObservedHeaderFrontier *ActionPlanFamilyObservedHeaderFrontier `json:"observed_header_frontier,omitempty"`
}

func cloneActionPlan(plan *ActionPlan) *ActionPlan {
	if plan == nil {
		return nil
	}
	out := &ActionPlan{
		Toolsets:                          maps.Clone(plan.Toolsets),
		Sources:                           slices.Clone(plan.Sources),
		Recipes:                           make(map[string]ActionRecipe, len(plan.Recipes)),
		Nodes:                             cloneActionPlanNodes(plan.Nodes),
		InputSets:                         cloneActionPlanInputSetNodes(plan.InputSets),
		Products:                          slices.Clone(plan.Products),
		projectedGeneratorValidations:     slices.Clone(plan.projectedGeneratorValidations),
		projectedGeneratorInternalNodes:   maps.Clone(plan.projectedGeneratorInternalNodes),
		projectedGeneratorInternalOutputs: maps.Clone(plan.projectedGeneratorInternalOutputs),
		projectedGeneratorOriginalOutputs: make(map[string]projectedGeneratorOriginalOutputCommitment, len(plan.projectedGeneratorOriginalOutputs)),
		projectedGeneratorCandidates:      make(map[string]projectedGeneratorCandidate, len(plan.projectedGeneratorCandidates)),
		metadata:                          plan.metadata,
	}
	for id, candidate := range plan.projectedGeneratorCandidates {
		candidate.ConfigProjectionPrefixes = slices.Clone(candidate.ConfigProjectionPrefixes)
		out.projectedGeneratorCandidates[id] = candidate
	}
	for id, commitment := range plan.projectedGeneratorOriginalOutputs {
		commitment.Outputs = slices.Clone(commitment.Outputs)
		out.projectedGeneratorOriginalOutputs[id] = commitment
	}
	for id, recipe := range plan.Recipes {
		out.Recipes[id] = cloneActionRecipe(recipe)
	}
	return out
}

func cloneActionPlanNodes(nodes []ActionPlanNode) []ActionPlanNode {
	out := slices.Clone(nodes)
	for index := range out {
		node := &out[index]
		node.Sources = slices.Clone(node.Sources)
		node.Inputs = slices.Clone(node.Inputs)
		node.Trees = slices.Clone(node.Trees)
		node.AuxiliaryTools = slices.Clone(node.AuxiliaryTools)
		node.Outputs = slices.Clone(node.Outputs)
		node.familySourceProjections = slices.Clone(node.familySourceProjections)
	}
	return out
}

// snapshotActionPlanView borrows the immutable snapshot maps and slices. It is
// safe only for read-only validation; snapshotActionPlan retains its existing
// detached-copy contract for reducers and tests which mutate the result.
func snapshotActionPlanView(snapshot ActionPlanSnapshot) *ActionPlan {
	return &ActionPlan{
		Toolsets:  snapshot.Toolsets,
		Sources:   snapshot.Sources,
		Recipes:   snapshot.Recipes,
		Nodes:     snapshot.Nodes,
		InputSets: snapshot.InputSets,
		Products:  snapshot.Products,
	}
}

func snapshotActionPlan(snapshot ActionPlanSnapshot) *ActionPlan {
	plan := snapshotActionPlanView(snapshot)
	internal := make(map[string]bool, len(snapshot.InternalNodes))
	for _, id := range snapshot.InternalNodes {
		internal[id] = true
	}
	internalOutputs := make(map[actionPlanOutputRef]bool, len(snapshot.InternalOutputs))
	for _, ref := range snapshot.InternalOutputs {
		internalOutputs[actionPlanOutputRef{producerID: ref.NodeID, slot: ref.Slot}] = true
	}
	commitments := make(map[string]projectedGeneratorOriginalOutputCommitment, len(snapshot.ProjectedGeneratorOutputs))
	for _, record := range snapshot.ProjectedGeneratorOutputs {
		commitments[record.NodeID] = projectedGeneratorOriginalOutputCommitment{
			TargetSlot: record.TargetSlot,
			Outputs:    record.Outputs,
		}
	}
	plan.projectedGeneratorInternalNodes = internal
	plan.projectedGeneratorInternalOutputs = internalOutputs
	plan.projectedGeneratorOriginalOutputs = commitments
	plan.projectedGeneratorValidations = snapshot.ValidationRoots
	return cloneActionPlan(plan)
}

func canonicalActionPlanSnapshot(plan *ActionPlan, dependencies map[string]ConfigDependencySet, configFiles map[string]string) (ActionPlanSnapshot, error) {
	if plan == nil {
		return ActionPlanSnapshot{}, fmt.Errorf("cannot snapshot a nil action plan")
	}
	inputSetStore, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return ActionPlanSnapshot{}, fmt.Errorf("load action plan snapshot input sets: %w", err)
	}
	inputSetRoots := make([]string, 0, len(plan.Nodes))
	for _, node := range plan.Nodes {
		inputSetRoots = append(inputSetRoots, node.InputSet)
	}
	inputSets, err := inputSetStore.ReachableNodesForRoots(inputSetRoots)
	if err != nil {
		return ActionPlanSnapshot{}, fmt.Errorf("snapshot input sets: %w", err)
	}
	snapshot := ActionPlanSnapshot{
		Schema:             LinuxKernelActionPlanSnapshotSchema,
		Toolsets:           maps.Clone(plan.Toolsets),
		Sources:            slices.Clone(plan.Sources),
		Recipes:            make(map[string]ActionRecipe, len(plan.Recipes)),
		Nodes:              cloneActionPlanNodes(plan.Nodes),
		InputSets:          inputSets,
		Products:           slices.Clone(plan.Products),
		ConfigDependencies: make(map[string]ConfigDependencySet, len(plan.Nodes)),
		ConfigFiles:        maps.Clone(configFiles),
		ValidationRoots:    slices.Clone(plan.projectedGeneratorValidations),
	}
	for id := range plan.projectedGeneratorInternalNodes {
		snapshot.InternalNodes = append(snapshot.InternalNodes, id)
	}
	for ref := range plan.projectedGeneratorInternalOutputs {
		snapshot.InternalOutputs = append(snapshot.InternalOutputs, ActionPlanSnapshotOutputRef{
			NodeID: ref.producerID,
			Slot:   ref.slot,
		})
	}
	for id, commitment := range plan.projectedGeneratorOriginalOutputs {
		snapshot.ProjectedGeneratorOutputs = append(snapshot.ProjectedGeneratorOutputs, ActionPlanSnapshotProjectedGeneratorOutputs{
			NodeID: id, TargetSlot: commitment.TargetSlot, Outputs: slices.Clone(commitment.Outputs),
		})
	}
	for id, recipe := range plan.Recipes {
		snapshot.Recipes[id] = cloneActionRecipe(recipe)
	}
	for _, node := range plan.Nodes {
		set, ok := dependencies[node.ID]
		if !ok {
			return ActionPlanSnapshot{}, fmt.Errorf("node %s has no config-dependency classification", node.ID)
		}
		canonical, err := CanonicalConfigDependencySet(set)
		if err != nil {
			return ActionPlanSnapshot{}, fmt.Errorf("node %s config dependencies: %w", node.ID, err)
		}
		snapshot.ConfigDependencies[node.ID] = canonical
	}
	for _, pathname := range ResolvedConfigProjectionOutputs() {
		if _, ok := snapshot.ConfigFiles[pathname]; !ok {
			return ActionPlanSnapshot{}, fmt.Errorf("action plan snapshot is missing resolved config projection %q", pathname)
		}
	}
	sort.Slice(snapshot.Sources, func(i, j int) bool { return snapshot.Sources[i].ID < snapshot.Sources[j].ID })
	sort.Slice(snapshot.Nodes, func(i, j int) bool { return snapshot.Nodes[i].ID < snapshot.Nodes[j].ID })
	sort.Slice(snapshot.Products, func(i, j int) bool { return snapshot.Products[i].Name < snapshot.Products[j].Name })
	sort.Strings(snapshot.InternalNodes)
	sort.Slice(snapshot.InternalOutputs, func(i, j int) bool {
		left, right := snapshot.InternalOutputs[i], snapshot.InternalOutputs[j]
		return left.NodeID < right.NodeID || left.NodeID == right.NodeID && left.Slot < right.Slot
	})
	sort.Slice(snapshot.ProjectedGeneratorOutputs, func(i, j int) bool {
		return snapshot.ProjectedGeneratorOutputs[i].NodeID < snapshot.ProjectedGeneratorOutputs[j].NodeID
	})
	sort.Strings(snapshot.ValidationRoots)
	// Validate the exact serialized representation once. Besides the standalone
	// action-plan contract this covers snapshot-only projected-generator proof
	// metadata, so the trusted writer can marshal without reconstructing and
	// validating the same graph a second time.
	if err := snapshot.validate(); err != nil {
		return ActionPlanSnapshot{}, fmt.Errorf("validate action plan snapshot: %w", err)
	}
	return snapshot, nil
}

func marshalCanonicalActionPlanSnapshot(s ActionPlanSnapshot) ([]byte, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode action plan snapshot: %w", err)
	}
	data = append(data, '\n')
	if len(data) > MaxActionPlanSnapshotBytes {
		return nil, fmt.Errorf("action plan snapshot contains %d bytes, want at most %d", len(data), MaxActionPlanSnapshotBytes)
	}
	return data, nil
}

func (s ActionPlanSnapshot) canonicalJSON() ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	return marshalCanonicalActionPlanSnapshot(s)
}

func (s ActionPlanSnapshot) validate() error {
	return s.validateWithStats(nil)
}

func (s ActionPlanSnapshot) validateWithStats(stats *actionPlanValidationStats) error {
	if s.Schema != LinuxKernelActionPlanSnapshotSchema {
		return fmt.Errorf("action plan snapshot schema %q, want %q", s.Schema, LinuxKernelActionPlanSnapshotSchema)
	}
	plan := snapshotActionPlanView(s)
	if err := plan.validateStructure(stats); err != nil {
		return fmt.Errorf("invalid snapshotted action plan: %w", err)
	}
	if len(s.ConfigDependencies) != len(plan.Nodes) {
		return fmt.Errorf("snapshot has %d config dependency records for %d nodes", len(s.ConfigDependencies), len(plan.Nodes))
	}
	nodes := make(map[string]ActionPlanNode, len(plan.Nodes))
	for _, node := range plan.Nodes {
		nodes[node.ID] = node
	}
	inputSets, err := plan.serializedActionPlanInputSetStore()
	if err != nil {
		return fmt.Errorf("snapshot input sets: %w", err)
	}
	internal := make(map[string]bool, len(s.InternalNodes))
	for name, ids := range map[string][]string{
		"internal_nodes": s.InternalNodes, "validation_roots": s.ValidationRoots,
	} {
		for index, id := range ids {
			if index != 0 && id <= ids[index-1] {
				return fmt.Errorf("snapshot %s are not in canonical unique order", name)
			}
			if _, ok := nodes[id]; !ok {
				return fmt.Errorf("snapshot %s references unknown node %s", name, id)
			}
			if name == "internal_nodes" {
				internal[id] = true
			}
		}
	}
	for _, id := range s.ValidationRoots {
		if !internal[id] {
			return fmt.Errorf("snapshot validation root %s is not internal", id)
		}
		if len(nodes[id].Outputs) != 1 || nodes[id].Outputs[0].ObservedPath != "" {
			return fmt.Errorf("snapshot validation root %s does not own one ordinary output", id)
		}
	}
	internalOutputs := map[actionPlanOutputRef]bool{}
	for index, ref := range s.InternalOutputs {
		if index != 0 {
			previous := s.InternalOutputs[index-1]
			if ref.NodeID < previous.NodeID || ref.NodeID == previous.NodeID && ref.Slot <= previous.Slot {
				return fmt.Errorf("snapshot internal_outputs are not in canonical unique order")
			}
		}
		node, ok := nodes[ref.NodeID]
		if !ok {
			return fmt.Errorf("snapshot internal_outputs references unknown node %s", ref.NodeID)
		}
		if ref.Slot < 0 || ref.Slot >= len(node.Outputs) {
			return fmt.Errorf("snapshot internal_outputs references absent slot %s[%d]", ref.NodeID, ref.Slot)
		}
		if !internal[ref.NodeID] {
			return fmt.Errorf("snapshot internal output %s[%d] belongs to a non-internal node", ref.NodeID, ref.Slot)
		}
		internalOutputs[actionPlanOutputRef{producerID: ref.NodeID, slot: ref.Slot}] = true
	}
	commitments := map[string]projectedGeneratorOriginalOutputCommitment{}
	for index, record := range s.ProjectedGeneratorOutputs {
		if index != 0 && record.NodeID <= s.ProjectedGeneratorOutputs[index-1].NodeID {
			return fmt.Errorf("snapshot projected_generator_outputs are not in canonical unique order")
		}
		node, ok := nodes[record.NodeID]
		if !ok {
			return fmt.Errorf("snapshot projected_generator_outputs references unknown node %s", record.NodeID)
		}
		if record.TargetSlot < 0 || record.TargetSlot >= len(record.Outputs) || len(record.Outputs) != len(node.Outputs) {
			return fmt.Errorf("snapshot projected-generator output commitment %s has invalid target/vector cardinality", record.NodeID)
		}
		commitments[record.NodeID] = projectedGeneratorOriginalOutputCommitment{
			TargetSlot: record.TargetSlot, Outputs: slices.Clone(record.Outputs),
		}
	}
	covered := map[string]bool{}
	coveredInputSets := map[string]bool{}
	var cover func(string) error
	var coverInputSet func(string) error
	coverInputSet = func(inputSetID string) error {
		if inputSetID == "" || coveredInputSets[inputSetID] {
			return nil
		}
		inputSetNode, ok := inputSets.Node(inputSetID)
		if !ok {
			return fmt.Errorf("snapshot references unknown input-set node %s", inputSetID)
		}
		coveredInputSets[inputSetID] = true
		for _, entry := range inputSetNode.Entries {
			if entry.ProducerID != "" {
				if err := cover(entry.ProducerID); err != nil {
					return err
				}
			}
		}
		for _, child := range inputSetNode.Children {
			if err := coverInputSet(child.ID); err != nil {
				return err
			}
		}
		return nil
	}
	cover = func(id string) error {
		if covered[id] {
			return nil
		}
		covered[id] = true
		for _, edge := range nodes[id].Inputs {
			if err := cover(edge.ProducerID); err != nil {
				return err
			}
		}
		if err := coverInputSet(nodes[id].InputSet); err != nil {
			return fmt.Errorf("snapshot node %s input set: %w", id, err)
		}
		return nil
	}
	for _, id := range s.ValidationRoots {
		if err := cover(id); err != nil {
			return err
		}
	}
	for _, id := range s.InternalNodes {
		if !covered[id] {
			return fmt.Errorf("snapshot internal node %s is not covered by a validation root", id)
		}
	}
	wantCanonicalRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{
			"-input", "${input:projection-raw:00000000}",
			"-validate_closed_integer_macro_header_v1",
			"-out", "${output:00000000}",
		},
		Inputs: []string{"projection-raw:00000000"}, Outputs: []string{"00000000"},
	}
	type canonicalPublisher struct {
		nodeID string
		count  int
	}
	canonicalPublishers := make(map[actionPlanOutputRef]canonicalPublisher, len(s.ValidationRoots))
	for _, candidate := range plan.Nodes {
		if len(candidate.Inputs) != 1 || candidate.Inputs[0].Role != "projection-raw" ||
			!reflect.DeepEqual(plan.Recipes[candidate.Recipe], wantCanonicalRecipe) {
			continue
		}
		key := actionPlanOutputRef{
			producerID: candidate.Inputs[0].ProducerID,
			slot:       candidate.Inputs[0].Slot,
		}
		publisher := canonicalPublishers[key]
		if publisher.count == 0 {
			publisher.nodeID = candidate.ID
		}
		publisher.count++
		canonicalPublishers[key] = publisher
	}
	validatedProjected := map[string]bool{}
	expectedInternal := map[string]bool{}
	expectedInternalOutputs := map[actionPlanOutputRef]bool{}
	expectedCommitments := map[string]bool{}
	allowedInternalConsumers := map[string]bool{}
	for _, id := range s.ValidationRoots {
		root := nodes[id]
		rootRecipe := plan.Recipes[root.Recipe]
		wantRootRecipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "metadata", Tool: "actionfile",
			Arguments: []string{
				"-compare_input", "${input:projected:00000000}",
				"-compare_input", "${input:full:00000001}",
				"-out", "${output:00000000}",
			},
			Inputs: []string{"projected:00000000", "full:00000001"}, Outputs: []string{"00000000"},
		}
		if root.Stage == "" || root.Kind != "metadata" || root.Tool != "actionfile" ||
			!reflect.DeepEqual(rootRecipe, wantRootRecipe) || len(root.Inputs) != 2 ||
			root.Inputs[0].Role != "projected" || root.Inputs[1].Role != "full" ||
			root.Inputs[0].Slot != root.Inputs[1].Slot {
			return fmt.Errorf("snapshot validation root %s is not the exact byte-comparator contract", id)
		}
		targetSlot := root.Inputs[0].Slot
		projected, projectedOK := nodes[root.Inputs[0].ProducerID]
		full, fullOK := nodes[root.Inputs[1].ProducerID]
		if !projectedOK || !fullOK || !internal[projected.ID] || !internal[full.ID] ||
			projected.Stage != full.Stage || projected.Kind != full.Kind || projected.Tool != full.Tool ||
			projected.Product != full.Product || !reflect.DeepEqual(projected.Sources, full.Sources) ||
			!reflect.DeepEqual(projected.Inputs, full.Inputs) || !slices.Equal(projected.Trees, full.Trees) ||
			!slices.Equal(projected.AuxiliaryTools, full.AuxiliaryTools) ||
			len(projected.Outputs) == 0 || len(projected.Outputs) != len(full.Outputs) ||
			targetSlot < 0 || targetSlot >= len(projected.Outputs) ||
			projected.Outputs[targetSlot].ObservedPath != "" || full.Outputs[targetSlot].ObservedPath != "" {
			return fmt.Errorf("snapshot validation root %s does not compare one exact projected/full target slot", id)
		}
		commitment, committed := commitments[projected.ID]
		if !committed || commitment.TargetSlot != targetSlot || len(commitment.Outputs) != len(projected.Outputs) {
			return fmt.Errorf("snapshot validation root %s has no exact original-output commitment", id)
		}
		projectedRecipe := plan.Recipes[projected.Recipe]
		fullRecipe := plan.Recipes[full.Recipe]
		if len(projectedRecipe.ConfigProjectionPrefixes) == 0 || len(fullRecipe.ConfigProjectionPrefixes) != 0 {
			return fmt.Errorf("snapshot validation root %s does not compare marked projected and unmarked full recipes", id)
		}
		expectedFull := cloneActionRecipe(projectedRecipe)
		expectedFull.ConfigProjectionPrefixes = nil
		if !reflect.DeepEqual(expectedFull, fullRecipe) {
			return fmt.Errorf("snapshot validation root %s full replay changes the tool-visible recipe", id)
		}
		if fullDependencies, ok := s.ConfigDependencies[full.ID]; !ok || !fullDependencies.Opaque {
			return fmt.Errorf("snapshot validation root %s full replay is not classified as full-config opaque", id)
		}

		publisher := canonicalPublishers[actionPlanOutputRef{producerID: projected.ID, slot: targetSlot}]
		if publisher.count > 1 {
			return fmt.Errorf("snapshot projected generator %s has multiple canonical target publishers", projected.ID)
		}
		canonical := nodes[publisher.nodeID]
		if canonical.ID == "" || internal[canonical.ID] || canonical.Stage != projected.Stage ||
			canonical.Kind != "generate" || canonical.Tool != "actionfile" || canonical.Product != projected.Product ||
			len(canonical.Sources) != 0 || len(canonical.Trees) != 0 || len(canonical.AuxiliaryTools) != 0 ||
			len(canonical.Outputs) != 1 || !actionPlanOutputIsCanonical(canonical.Outputs[0]) ||
			canonical.Outputs[0].ObservedPath != "" {
			return fmt.Errorf("snapshot projected generator %s has no exact canonical target publisher", projected.ID)
		}

		originalOutputs := slices.Clone(commitment.Outputs)
		if !reflect.DeepEqual(canonical.Outputs[0], originalOutputs[targetSlot]) {
			return fmt.Errorf("snapshot projected generator %s canonical target disagrees with its original-output commitment", projected.ID)
		}
		identity := compactKbuildProjectedGeneratorIdentity(originalOutputs, targetSlot)
		for slot, original := range originalOutputs {
			wantProjected := original
			wantProjected.ArtifactPath = path.Join(
				compactKbuildProjectedFilechkRoot, identity, "projected", planOrdinal(slot),
			)
			if !reflect.DeepEqual(projected.Outputs[slot], wantProjected) {
				return fmt.Errorf("snapshot projected generator %s output slot %d is not the exact private raw descriptor", projected.ID, slot)
			}
			wantFull := original
			if slot == targetSlot {
				wantFull.ArtifactPath = path.Join(
					compactKbuildProjectionValidateRoot, identity, "full", planOrdinal(slot),
				)
			}
			if !reflect.DeepEqual(full.Outputs[slot], wantFull) {
				return fmt.Errorf("snapshot full replay %s output slot %d changes its original descriptor", full.ID, slot)
			}
			expectedInternalOutputs[actionPlanOutputRef{producerID: projected.ID, slot: slot}] = true
		}
		wantStamp := ActionPlanOutput{
			Tree: originalOutputs[targetSlot].Tree,
			Path: path.Join(compactKbuildProjectionValidateRoot, identity, "validated"),
		}
		if root.Stage != projected.Stage || root.Product != projected.Product ||
			len(root.Sources) != 0 || len(root.Trees) != 0 || len(root.AuxiliaryTools) != 0 ||
			len(root.Outputs) != 1 || !reflect.DeepEqual(root.Outputs[0], wantStamp) ||
			!reflect.DeepEqual(canonical.Outputs[0], originalOutputs[targetSlot]) {
			return fmt.Errorf("snapshot validation root %s changes the target publication envelope", id)
		}
		expectedInternal[projected.ID] = true
		expectedInternal[full.ID] = true
		expectedInternal[root.ID] = true
		expectedInternalOutputs[actionPlanOutputRef{producerID: full.ID, slot: targetSlot}] = true
		expectedInternalOutputs[actionPlanOutputRef{producerID: root.ID, slot: 0}] = true
		expectedCommitments[projected.ID] = true
		allowedInternalConsumers[canonical.ID+"\x00"+planOrdinal(0)] = true
		allowedInternalConsumers[root.ID+"\x00"+planOrdinal(0)] = true
		allowedInternalConsumers[root.ID+"\x00"+planOrdinal(1)] = true
		validatedProjected[projected.ID] = true
	}
	if !maps.Equal(internal, expectedInternal) {
		return fmt.Errorf("snapshot internal_nodes do not equal the exact differential validation nodes")
	}
	if !maps.Equal(internalOutputs, expectedInternalOutputs) {
		return fmt.Errorf("snapshot internal_outputs do not equal the exact differential validation slots")
	}
	if len(commitments) != len(expectedCommitments) {
		return fmt.Errorf("snapshot projected_generator_outputs do not equal the exact validated generator set")
	}
	for id := range commitments {
		if !expectedCommitments[id] {
			return fmt.Errorf("snapshot has unexpected projected-generator output commitment %s", id)
		}
	}
	type internalOutputHit struct {
		ref   actionPlanOutputRef
		found bool
	}
	internalOutputKnown := map[string]bool{}
	internalOutputHits := map[string]internalOutputHit{}
	var inputSetInternalOutput func(string) (internalOutputHit, error)
	inputSetInternalOutput = func(inputSetID string) (internalOutputHit, error) {
		if inputSetID == "" {
			return internalOutputHit{}, nil
		}
		if internalOutputKnown[inputSetID] {
			return internalOutputHits[inputSetID], nil
		}
		inputSetNode, ok := inputSets.Node(inputSetID)
		if !ok {
			return internalOutputHit{}, fmt.Errorf("snapshot references unknown input-set node %s", inputSetID)
		}
		hit := internalOutputHit{}
		for _, entry := range inputSetNode.Entries {
			ref := actionPlanOutputRef{producerID: entry.ProducerID, slot: entry.Slot}
			if entry.ProducerID != "" && internalOutputs[ref] {
				hit = internalOutputHit{ref: ref, found: true}
				break
			}
		}
		if !hit.found {
			for _, child := range inputSetNode.Children {
				childHit, err := inputSetInternalOutput(child.ID)
				if err != nil {
					return internalOutputHit{}, err
				}
				if childHit.found {
					hit = childHit
					break
				}
			}
		}
		internalOutputKnown[inputSetID] = true
		internalOutputHits[inputSetID] = hit
		return hit, nil
	}
	for _, consumer := range plan.Nodes {
		for edgeIndex, input := range consumer.Inputs {
			if !internalOutputs[actionPlanOutputRef{producerID: input.ProducerID, slot: input.Slot}] {
				continue
			}
			if !allowedInternalConsumers[consumer.ID+"\x00"+planOrdinal(edgeIndex)] {
				return fmt.Errorf("snapshot node %s illegally consumes internal projected output %s[%d]", consumer.ID, input.ProducerID, input.Slot)
			}
		}
		if len(internalOutputs) != 0 {
			hit, err := inputSetInternalOutput(consumer.InputSet)
			if err != nil {
				return err
			}
			if hit.found {
				return fmt.Errorf("snapshot node %s illegally consumes internal projected output %s[%d] through its input set", consumer.ID, hit.ref.producerID, hit.ref.slot)
			}
		}
	}
	for _, node := range plan.Nodes {
		recipe := plan.Recipes[node.Recipe]
		if len(recipe.ConfigProjectionPrefixes) == 0 {
			continue
		}
		if !internal[node.ID] || !covered[node.ID] || !validatedProjected[node.ID] {
			return fmt.Errorf("snapshot projected generator %s is not internal and exactly differential-validated", node.ID)
		}
	}
	for _, node := range plan.Nodes {
		set, ok := s.ConfigDependencies[node.ID]
		if !ok {
			return fmt.Errorf("snapshot node %s has no config dependency record", node.ID)
		}
		canonical, err := CanonicalConfigDependencySet(set)
		if err != nil {
			return fmt.Errorf("snapshot node %s config dependencies: %w", node.ID, err)
		}
		if !reflect.DeepEqual(canonical, set) {
			return fmt.Errorf("snapshot node %s config dependencies are not canonical", node.ID)
		}
	}
	for _, pathname := range ResolvedConfigProjectionOutputs() {
		if _, ok := s.ConfigFiles[pathname]; !ok {
			return fmt.Errorf("snapshot is missing resolved config projection %q", pathname)
		}
	}
	if len(s.ConfigFiles) != len(ResolvedConfigProjectionOutputs()) {
		return fmt.Errorf("snapshot has %d resolved config projections, want %d", len(s.ConfigFiles), len(ResolvedConfigProjectionOutputs()))
	}
	return nil
}

var errActionPlanSnapshotCompressedTooLarge = errors.New("compressed action plan snapshot exceeds its byte limit")

type actionPlanSnapshotBoundedBuffer struct {
	bytes.Buffer
	limit int64
}

func (b *actionPlanSnapshotBoundedBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - int64(b.Len())
	if remaining <= 0 {
		return 0, errActionPlanSnapshotCompressedTooLarge
	}
	if int64(len(data)) > remaining {
		written, _ := b.Buffer.Write(data[:remaining])
		return written, errActionPlanSnapshotCompressedTooLarge
	}
	return b.Buffer.Write(data)
}

func compressCanonicalActionPlanSnapshot(data []byte, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("compressed action plan snapshot byte limit must be positive")
	}
	output := actionPlanSnapshotBoundedBuffer{limit: limit}
	writer, err := gzip.NewWriterLevel(&output, gzip.BestSpeed)
	if err != nil {
		return nil, fmt.Errorf("create action plan snapshot gzip writer: %w", err)
	}
	// Keep every optional header field empty and the OS value platform-neutral.
	// In particular, the zero ModTime prevents wall-clock bytes from entering
	// the transport. BestSpeed keeps final-planner CPU bounded while retaining
	// most of the large ratio available from the repetitive canonical JSON.
	writer.Header = gzip.Header{OS: 255}
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		if errors.Is(err, errActionPlanSnapshotCompressedTooLarge) {
			return nil, fmt.Errorf("compressed action plan snapshot exceeds %d bytes", limit)
		}
		return nil, fmt.Errorf("compress action plan snapshot: %w", err)
	}
	if err := writer.Close(); err != nil {
		if errors.Is(err, errActionPlanSnapshotCompressedTooLarge) {
			return nil, fmt.Errorf("compressed action plan snapshot exceeds %d bytes", limit)
		}
		return nil, fmt.Errorf("finish action plan snapshot gzip stream: %w", err)
	}
	return slices.Clone(output.Bytes()), nil
}

// WriteActionPlanSnapshot publishes a bounded, canonical, lossless variant
// plan as deterministic gzip. configFiles are keyed by
// ResolvedConfigProjectionOutputs paths.
func WriteActionPlanSnapshot(output string, plan *ActionPlan, dependencies map[string]ConfigDependencySet, configFiles map[string]string) error {
	snapshot, err := canonicalActionPlanSnapshot(plan, dependencies, configFiles)
	if err != nil {
		return err
	}
	data, err := marshalCanonicalActionPlanSnapshot(snapshot)
	if err != nil {
		return err
	}
	compressed, err := compressCanonicalActionPlanSnapshot(data, MaxActionPlanSnapshotCompressedBytes)
	if err != nil {
		return err
	}
	return writeCanonicalFile(output, compressed)
}

func ReadActionPlanSnapshot(filename string) (ActionPlanSnapshot, error) {
	return readActionPlanSnapshotWithLimits(filename, MaxActionPlanSnapshotCompressedBytes, MaxActionPlanSnapshotBytes)
}

type actionPlanSnapshotDecompressionRecorder struct {
	reader io.Reader
	err    error
}

func (r *actionPlanSnapshotDecompressionRecorder) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
}

// readActionPlanSnapshotMember exposes one bounded decompressed member to
// consume, then drains it before returning. Draining is part of validation: a
// JSON decoder may stop at a syntax error or after one complete value, while
// the gzip checksum and exact single-member/trailing-byte contract live at the
// end of the stream.
func readActionPlanSnapshotMember(
	file *os.File,
	compressedLimit, decompressedLimit int64,
	consume func(io.Reader) error,
) (int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("rewind compressed action plan snapshot: %w", err)
	}
	compressed := &io.LimitedReader{R: file, N: compressedLimit + 1}
	// gzip.Reader promises not to consume bytes after the selected member when
	// its input implements io.ByteReader. Keep that boundary explicit so we can
	// count, rather than buffer, every concatenated-member or trailing byte.
	buffered := bufio.NewReader(compressed)
	reader, err := gzip.NewReader(buffered)
	if err != nil {
		return 0, fmt.Errorf("open action plan snapshot gzip stream: %w", err)
	}
	reader.Multistream(false)
	recorded := &actionPlanSnapshotDecompressionRecorder{reader: reader}
	decompressed := &io.LimitedReader{R: recorded, N: decompressedLimit + 1}
	consumeErr := consume(decompressed)
	_, drainErr := io.Copy(io.Discard, decompressed)
	decompressedBytes := decompressedLimit + 1 - decompressed.N
	closeErr := reader.Close()
	trailingBytes, trailingErr := io.Copy(io.Discard, buffered)
	compressedBytes := compressedLimit + 1 - compressed.N

	// os.File may have grown since it was statted. Preserve the compressed
	// ceiling without ever reading more than one byte beyond it.
	if compressedBytes > compressedLimit {
		return decompressedBytes, fmt.Errorf("compressed action plan snapshot contains more than %d bytes", compressedLimit)
	}
	if recorded.err != nil {
		return decompressedBytes, fmt.Errorf("decompress action plan snapshot: %w", recorded.err)
	}
	if drainErr != nil {
		return decompressedBytes, fmt.Errorf("decompress action plan snapshot: %w", drainErr)
	}
	if closeErr != nil {
		return decompressedBytes, fmt.Errorf("close action plan snapshot gzip stream: %w", closeErr)
	}
	if decompressedBytes > decompressedLimit {
		return decompressedBytes, fmt.Errorf("decompressed action plan snapshot contains more than %d bytes", decompressedLimit)
	}
	if trailingErr != nil {
		return decompressedBytes, fmt.Errorf("read trailing action plan snapshot bytes: %w", trailingErr)
	}
	if trailingBytes != 0 {
		return decompressedBytes, fmt.Errorf("action plan snapshot has %d trailing compressed bytes", trailingBytes)
	}
	if decompressedBytes == 0 {
		return 0, fmt.Errorf("decompressed action plan snapshot is empty")
	}
	if consumeErr != nil {
		return decompressedBytes, consumeErr
	}
	return decompressedBytes, nil
}

var errActionPlanSnapshotNotCanonical = errors.New("action plan snapshot is not canonically encoded")

// actionPlanSnapshotCanonicalComparison receives canonical bytes and consumes
// the same number of bytes from the second decompression pass. Its fixed-size
// scratch space avoids retaining either representation of the complete JSON.
type actionPlanSnapshotCanonicalComparison struct {
	actual  io.Reader
	scratch [32 << 10]byte
	bytes   int64
}

func (w *actionPlanSnapshotCanonicalComparison) Write(expected []byte) (int, error) {
	w.bytes += int64(len(expected))
	written := 0
	for len(expected) != 0 {
		chunk := min(len(expected), len(w.scratch))
		n, err := io.ReadFull(w.actual, w.scratch[:chunk])
		if n != 0 && !bytes.Equal(expected[:n], w.scratch[:n]) {
			return written + n, errActionPlanSnapshotNotCanonical
		}
		written += n
		expected = expected[n:]
		if err != nil {
			return written, errActionPlanSnapshotNotCanonical
		}
	}
	return written, nil
}

func writeActionPlanSnapshotCanonicalJSON(output io.Writer, snapshot ActionPlanSnapshot) error {
	if err := writeActionPlanSnapshotCanonicalValue(output, reflect.ValueOf(snapshot)); err != nil {
		return fmt.Errorf("encode action plan snapshot: %w", err)
	}
	if _, err := io.WriteString(output, "\n"); err != nil {
		return fmt.Errorf("encode action plan snapshot: %w", err)
	}
	return nil
}

func writeActionPlanSnapshotCanonicalBytes(output io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := output.Write(data)
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func actionPlanSnapshotJSONEmpty(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return value.Len() == 0
	case reflect.Bool:
		return !value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return value.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return value.IsNil()
	default:
		return false
	}
}

// writeActionPlanSnapshotCanonicalValue is the streaming counterpart of
// encoding/json's compact encoding for the closed ActionPlanSnapshot data
// model. Containers are traversed directly, map keys are sorted, and only one
// scalar string is marshaled at a time. The differential test beside the
// transport tests keeps this byte-for-byte locked to encoding/json.
func writeActionPlanSnapshotCanonicalValue(output io.Writer, value reflect.Value) error {
	if !value.IsValid() {
		return writeActionPlanSnapshotCanonicalBytes(output, []byte("null"))
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return writeActionPlanSnapshotCanonicalBytes(output, []byte("null"))
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Struct:
		if err := writeActionPlanSnapshotCanonicalBytes(output, []byte{'{'}); err != nil {
			return err
		}
		first := true
		valueType := value.Type()
		for index := 0; index < value.NumField(); index++ {
			fieldType := valueType.Field(index)
			if fieldType.PkgPath != "" {
				continue
			}
			tag := fieldType.Tag.Get("json")
			parts := strings.Split(tag, ",")
			if parts[0] == "-" {
				continue
			}
			field := value.Field(index)
			omitEmpty := slices.Contains(parts[1:], "omitempty")
			if omitEmpty && actionPlanSnapshotJSONEmpty(field) {
				continue
			}
			name := parts[0]
			if name == "" {
				name = fieldType.Name
			}
			if !first {
				if err := writeActionPlanSnapshotCanonicalBytes(output, []byte{','}); err != nil {
					return err
				}
			}
			first = false
			encodedName, err := json.Marshal(name)
			if err != nil {
				return err
			}
			if err := writeActionPlanSnapshotCanonicalBytes(output, encodedName); err != nil {
				return err
			}
			if err := writeActionPlanSnapshotCanonicalBytes(output, []byte{':'}); err != nil {
				return err
			}
			if err := writeActionPlanSnapshotCanonicalValue(output, field); err != nil {
				return err
			}
		}
		return writeActionPlanSnapshotCanonicalBytes(output, []byte{'}'})
	case reflect.Map:
		if value.IsNil() {
			return writeActionPlanSnapshotCanonicalBytes(output, []byte("null"))
		}
		if value.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("unsupported action plan snapshot map key type %s", value.Type().Key())
		}
		if err := writeActionPlanSnapshotCanonicalBytes(output, []byte{'{'}); err != nil {
			return err
		}
		keys := make([]string, 0, value.Len())
		for _, key := range value.MapKeys() {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		for index, key := range keys {
			if index != 0 {
				if err := writeActionPlanSnapshotCanonicalBytes(output, []byte{','}); err != nil {
					return err
				}
			}
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return err
			}
			if err := writeActionPlanSnapshotCanonicalBytes(output, encodedKey); err != nil {
				return err
			}
			if err := writeActionPlanSnapshotCanonicalBytes(output, []byte{':'}); err != nil {
				return err
			}
			mapKey := reflect.New(value.Type().Key()).Elem()
			mapKey.SetString(key)
			if err := writeActionPlanSnapshotCanonicalValue(output, value.MapIndex(mapKey)); err != nil {
				return err
			}
		}
		return writeActionPlanSnapshotCanonicalBytes(output, []byte{'}'})
	case reflect.Slice:
		if value.IsNil() {
			return writeActionPlanSnapshotCanonicalBytes(output, []byte("null"))
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			encoded, err := json.Marshal(value.Bytes())
			if err != nil {
				return err
			}
			return writeActionPlanSnapshotCanonicalBytes(output, encoded)
		}
		fallthrough
	case reflect.Array:
		if err := writeActionPlanSnapshotCanonicalBytes(output, []byte{'['}); err != nil {
			return err
		}
		for index := 0; index < value.Len(); index++ {
			if index != 0 {
				if err := writeActionPlanSnapshotCanonicalBytes(output, []byte{','}); err != nil {
					return err
				}
			}
			if err := writeActionPlanSnapshotCanonicalValue(output, value.Index(index)); err != nil {
				return err
			}
		}
		return writeActionPlanSnapshotCanonicalBytes(output, []byte{']'})
	case reflect.String:
		encoded, err := json.Marshal(value.String())
		if err != nil {
			return err
		}
		return writeActionPlanSnapshotCanonicalBytes(output, encoded)
	case reflect.Bool:
		return writeActionPlanSnapshotCanonicalBytes(output, []byte(strconv.FormatBool(value.Bool())))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return writeActionPlanSnapshotCanonicalBytes(output, []byte(strconv.FormatInt(value.Int(), 10)))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return writeActionPlanSnapshotCanonicalBytes(output, []byte(strconv.FormatUint(value.Uint(), 10)))
	case reflect.Float32, reflect.Float64:
		encoded, err := json.Marshal(value.Interface())
		if err != nil {
			return err
		}
		return writeActionPlanSnapshotCanonicalBytes(output, encoded)
	default:
		return fmt.Errorf("unsupported action plan snapshot JSON value %s", value.Type())
	}
}

func readActionPlanSnapshotWithLimits(filename string, compressedLimit, decompressedLimit int64) (ActionPlanSnapshot, error) {
	if compressedLimit <= 0 || decompressedLimit <= 0 {
		return ActionPlanSnapshot{}, fmt.Errorf("action plan snapshot byte limits must be positive")
	}
	file, err := os.Open(filename)
	if err != nil {
		return ActionPlanSnapshot{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ActionPlanSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return ActionPlanSnapshot{}, fmt.Errorf("action plan snapshot %q is not a regular file", filename)
	}
	if info.Size() <= 0 || info.Size() > compressedLimit {
		return ActionPlanSnapshot{}, fmt.Errorf("compressed action plan snapshot contains %d bytes, want 1..%d", info.Size(), compressedLimit)
	}
	var snapshot ActionPlanSnapshot
	if _, err := readActionPlanSnapshotMember(file, compressedLimit, decompressedLimit, func(input io.Reader) error {
		decoder := json.NewDecoder(input)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&snapshot); err != nil {
			return fmt.Errorf("decode action plan snapshot: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			if err == nil {
				return fmt.Errorf("decode action plan snapshot: trailing JSON value")
			}
			return fmt.Errorf("decode action plan snapshot: %w", err)
		}
		return nil
	}); err != nil {
		return ActionPlanSnapshot{}, err
	}
	if err := snapshot.validate(); err != nil {
		return ActionPlanSnapshot{}, err
	}
	if _, err := readActionPlanSnapshotMember(file, compressedLimit, decompressedLimit, func(input io.Reader) error {
		comparison := &actionPlanSnapshotCanonicalComparison{actual: input}
		bufferedComparison := bufio.NewWriterSize(comparison, len(comparison.scratch))
		if err := writeActionPlanSnapshotCanonicalJSON(bufferedComparison, snapshot); err != nil {
			if errors.Is(err, errActionPlanSnapshotNotCanonical) {
				return errActionPlanSnapshotNotCanonical
			}
			return err
		}
		if err := bufferedComparison.Flush(); err != nil {
			if errors.Is(err, errActionPlanSnapshotNotCanonical) {
				return errActionPlanSnapshotNotCanonical
			}
			return err
		}
		if comparison.bytes > MaxActionPlanSnapshotBytes {
			return fmt.Errorf("action plan snapshot contains %d bytes, want at most %d", comparison.bytes, MaxActionPlanSnapshotBytes)
		}
		var trailing [1]byte
		n, err := input.Read(trailing[:])
		if n != 0 || err == nil {
			return errActionPlanSnapshotNotCanonical
		}
		if !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}); err != nil {
		return ActionPlanSnapshot{}, err
	}
	return snapshot, nil
}

func semanticFamilySourceID(namespace, pathname string) string {
	hash := sha256.New()
	hash.Write([]byte("linux-kernel-family-source-v1\x00"))
	hash.Write([]byte(namespace))
	hash.Write([]byte{0})
	hash.Write([]byte(pathname))
	return "src-" + hex.EncodeToString(hash.Sum(nil))
}

func plannerOwnedPrivateFamilyArtifact(output ActionPlanOutput) (string, bool) {
	if output.ArtifactPath == "" {
		return "", false
	}
	if relative, owned := strings.CutPrefix(output.ArtifactPath, familySelectionArtifactDirectory+"/"); owned {
		selectionID, pathname, complete := strings.Cut(relative, "/")
		if complete && validatePlanDigest("Kbuild selection artifact", selectionID) == nil && pathname == output.Path {
			return familySelectionArtifactDirectory, true
		}
	}
	for _, prefix := range []string{
		familyIntermediateArtifactDirectory,
		familySideOutputArtifactDirectory,
		familySideOutputResolutionArtifactDirectory,
	} {
		if output.ArtifactPath == prefix || strings.HasPrefix(output.ArtifactPath, prefix+"/") {
			return prefix, true
		}
	}
	return "", false
}

func structuralFamilyArtifactPath(output ActionPlanOutput) string {
	if plannerOwnedObservedFamilyState(output) {
		return path.Join(compactKbuildSideOutputStateDirectory, "family-normalized.state")
	}
	prefix, private := plannerOwnedPrivateFamilyArtifact(output)
	if !private {
		return actionPlanOutputArtifactPath(output)
	}
	// Preserve which allocator produced the private path, but erase its whole
	// allocation below that prefix. Output.Path and the output ordinal retain
	// the Kbuild-visible identity without admitting a profile digest as a salt.
	return path.Join(prefix, "family-normalized")
}

func plannerOwnedObservedFamilyState(output ActionPlanOutput) bool {
	if output.ObservedPath == "" {
		return false
	}
	relative, owned := strings.CutPrefix(output.Path, compactKbuildSideOutputStateDirectory+"/")
	if !owned {
		return false
	}
	components := strings.Split(relative, "/")
	if len(components) == 2 && components[0] == "commands" {
		components = components[1:]
	}
	if len(components) != 1 {
		return false
	}
	digest, state := strings.CutSuffix(components[0], ".state")
	return state && validatePlanDigest("observed-state allocation", digest) == nil
}

func structuralFamilyOutputPath(output ActionPlanOutput) string {
	if plannerOwnedObservedFamilyState(output) {
		return path.Join(compactKbuildSideOutputStateDirectory, "family-normalized.state")
	}
	return output.Path
}

func familyOwnedArtifactPath(structuralNodeID string, slot int) string {
	return path.Join(familyOwnedArtifactDirectory, structuralNodeID, planOrdinal(slot))
}

func familyOwnedObservedStatePath(structuralNodeID string, slot int) string {
	return path.Join(familyOwnedObservedStateDirectory, structuralNodeID, planOrdinal(slot)+".state")
}

func semanticFamilyNodeBytes(
	node ActionPlanNode,
	sourceIDs []string,
	producerIDs []string,
	inputSetID string,
	outputPathIdentity func(ActionPlanOutput) string,
	artifactPathIdentity func(ActionPlanOutput) string,
) []byte {
	var out bytes.Buffer
	write := func(values ...string) {
		for _, value := range values {
			out.WriteString(strconv.Itoa(len(value)))
			out.WriteByte(':')
			out.WriteString(value)
		}
		out.WriteByte(0)
	}
	write("linux-kernel-family-action-node-v3")
	// Product is facade metadata and intentionally does not participate. The
	// supplied artifact-path identity does: final identities use the relocated
	// canonical path exposed as a consumer's ProjectionPath, while structural
	// identities erase only planner-owned allocations. Stage remains semantic
	// because it selects host or target execution authority.
	write(node.Stage, node.Kind, node.Recipe, node.Tool)
	for index, source := range node.Sources {
		write("source", planOrdinal(index), source.Role, sourceIDs[index])
	}
	for index, input := range node.Inputs {
		write("input", planOrdinal(index), input.Role, producerIDs[index], planOrdinal(input.Slot))
	}
	if inputSetID != "" {
		write("input-set", inputSetID)
	}
	for _, tree := range node.Trees {
		write("tree", tree)
	}
	for _, tool := range node.AuxiliaryTools {
		write("auxiliary-tool", tool)
	}
	for index, projection := range node.familySourceProjections {
		write(
			"source-projection", planOrdinal(index), planOrdinal(projection.SourceOrdinal),
			projection.Tree, projection.Path,
		)
	}
	for index, output := range node.Outputs {
		write("output", planOrdinal(index), output.Tree, outputPathIdentity(output), artifactPathIdentity(output), output.ObservedPath)
	}
	return out.Bytes()
}

type familyVariantReduction struct {
	name                    string
	plan                    *ActionPlan
	dependencies            map[string]ConfigDependencySet
	configFiles             map[string]string
	structuralID            map[string]string
	structuralInputSetRoots map[string]string
	hasConfigSource         map[string]bool
	finalID                 map[string]string
	views                   []ActionPlanFamilyView
	products                []ActionPlanFamilyProduct
	validations             []ActionPlanFamilyValidation
}

type familyStructuralCompatibilityNode struct {
	reductionIndex int
	nodeID         string
	baseID         string
	dependencies   ConfigDependencySet
	fullConfigID   string
}

const directNonCompilerConfigProjectionReason = "direct non-compiler config projection consumer"

// familyNodeRequiresFullConfig reports the effective capsule boundary used by
// nodeCapsule below. Most callers carry that fact directly in their dependency
// annotation. A direct, untyped config-projection consumer is also fail-closed:
// its argv does not prove which bytes it reads, so it must be partitioned like
// an explicitly opaque member even when its local annotation is precise.
func familyNodeRequiresFullConfig(
	plan *ActionPlan,
	node ActionPlanNode,
	dependencies ConfigDependencySet,
	hasConfigSource bool,
) bool {
	if dependencies.Opaque {
		return true
	}
	recipe, recipeOK := plan.Recipes[node.Recipe]
	projectedGenerator := recipeOK && len(recipe.ConfigProjectionPrefixes) != 0
	return hasConfigSource &&
		!preciseFamilyCompilerNode(plan, node, dependencies) &&
		!knownConfigFreeFamilyNode(plan, node) && !projectedGenerator
}

func effectiveFamilyNodeConfigDependencies(
	plan *ActionPlan,
	node ActionPlanNode,
	dependencies ConfigDependencySet,
	hasConfigSource bool,
) ConfigDependencySet {
	if dependencies.Opaque || !familyNodeRequiresFullConfig(plan, node, dependencies, hasConfigSource) {
		return dependencies
	}
	dependencies.Opaque = true
	dependencies.Reason = directNonCompilerConfigProjectionReason
	return dependencies
}

func derivedFamilyStructuralCompatibilityID(
	baseID, fullConfigID string,
	node ActionPlanNode,
	producerIDs []string,
	inputSetID string,
) string {
	var payload bytes.Buffer
	write := func(values ...string) {
		for _, value := range values {
			payload.WriteString(strconv.Itoa(len(value)))
			payload.WriteByte(':')
			payload.WriteString(value)
		}
		payload.WriteByte(0)
	}
	write("linux-kernel-family-structural-compatibility-v1", baseID)
	if fullConfigID == "" {
		write("precise")
	} else {
		write("opaque", fullConfigID)
	}
	for index, input := range node.Inputs {
		write(
			"input", planOrdinal(index), input.Role,
			producerIDs[index], planOrdinal(input.Slot),
		)
	}
	if inputSetID != "" {
		write("input-set", inputSetID)
	}
	digest := sha256.Sum256(payload.Bytes())
	return hex.EncodeToString(digest[:])
}

// partitionFamilyStructuralCompatibility refines config-erased structural
// classes before their dependency sets are unioned. A non-opaque class keeps
// its original structural ID, preserving the exact family output allocation
// and semantic identity used before an opaque sibling was introduced. Opaque
// members instead carry their complete rendered config identity and therefore
// cannot broaden that precise class. Two opaque members may still share when
// their structural execution graph and complete config bytes agree.
//
// Explicit producer subgroup IDs participate transitively. Resolving the DAG
// recursively is the deterministic fixed point of repeatedly splitting a
// structural class whenever one of its producer classes splits. Consequently
// an opaque producer also separates its consumers without making the result
// depend on variant or map iteration order.
func partitionFamilyStructuralCompatibility(
	reductions []familyVariantReduction,
) (map[string]ConfigDependencySet, error) {
	records := make([][]familyStructuralCompatibilityNode, len(reductions))
	nodeOrdinals := make([]map[string]int, len(reductions))
	inputSetStores := make([]*ActionPlanInputSetStore, len(reductions))
	sourceIndexes := make([]map[string]ActionPlanSource, len(reductions))
	fullConfigIDs := make([]string, len(reductions))

	for reductionIndex := range reductions {
		reduction := &reductions[reductionIndex]
		records[reductionIndex] = make([]familyStructuralCompatibilityNode, len(reduction.plan.Nodes))
		nodeOrdinals[reductionIndex] = make(map[string]int, len(reduction.plan.Nodes))
		sources := make(map[string]ActionPlanSource, len(reduction.plan.Sources))
		for _, source := range reduction.plan.Sources {
			sources[source.ID] = source
		}
		sourceIndexes[reductionIndex] = sources
		store, err := reduction.plan.planningActionPlanInputSetStore()
		if err != nil {
			return nil, fmt.Errorf("variant %s input sets: %w", reduction.name, err)
		}
		inputSetStores[reductionIndex] = store
		for nodeIndex, node := range reduction.plan.Nodes {
			nodeOrdinals[reductionIndex][node.ID] = nodeIndex
			baseID := reduction.structuralID[node.ID]
			if baseID == "" {
				return nil, fmt.Errorf("variant %s node %s has no base structural identity", reduction.name, node.ID)
			}
			dependencies, err := CanonicalConfigDependencySet(reduction.dependencies[node.ID])
			if err != nil {
				return nil, fmt.Errorf("variant %s node %s config dependencies: %w", reduction.name, node.ID, err)
			}
			fullConfigID := ""
			if familyNodeRequiresFullConfig(reduction.plan, node, dependencies, reduction.hasConfigSource[node.ID]) {
				if fullConfigIDs[reductionIndex] == "" {
					capsule, err := RenderConfigCapsule(reduction.configFiles, ConfigDependencySet{
						Opaque: true, Reason: "family structural compatibility",
					})
					if err != nil {
						return nil, fmt.Errorf("variant %s full-config compatibility capsule: %w", reduction.name, err)
					}
					fullConfigIDs[reductionIndex] = capsule.ID
				}
				fullConfigID = fullConfigIDs[reductionIndex]
			}
			records[reductionIndex][nodeIndex] = familyStructuralCompatibilityNode{
				reductionIndex: reductionIndex,
				nodeID:         node.ID,
				baseID:         baseID,
				dependencies:   dependencies,
				fullConfigID:   fullConfigID,
			}
		}
	}

	resolved := make([][]string, len(reductions))
	stack := make([][]bool, len(reductions))
	for reductionIndex := range reductions {
		resolved[reductionIndex] = make([]string, len(records[reductionIndex]))
		stack[reductionIndex] = make([]bool, len(records[reductionIndex]))
	}
	var resolve func(int, int) (string, error)
	inputSetMappers := make([]*ActionPlanInputSetTargetStableMapper, len(reductions))
	for index := range reductions {
		reductionIndex := index
		mapper, err := inputSetStores[reductionIndex].NewTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
			if entry.SourceID != "" {
				source, ok := sourceIndexes[reductionIndex][entry.SourceID]
				if !ok {
					return ActionPlanInputSetEntry{}, fmt.Errorf("variant %s compatibility input set references unknown source %s", reductions[reductionIndex].name, entry.SourceID)
				}
				identity := fullConfigSourceIdentity(source)
				if !validFamilySourceID(identity) {
					identity = semanticFamilySourceID("input-set-structural", identity)
				}
				entry.SourceID = identity
				return entry, nil
			}
			producerIndex, ok := nodeOrdinals[reductionIndex][entry.ProducerID]
			if !ok {
				return ActionPlanInputSetEntry{}, fmt.Errorf("variant %s compatibility input set references unknown producer %s", reductions[reductionIndex].name, entry.ProducerID)
			}
			producerID, err := resolve(reductionIndex, producerIndex)
			if err != nil {
				return ActionPlanInputSetEntry{}, err
			}
			entry.ProducerID = producerID
			return entry, nil
		})
		if err != nil {
			return nil, fmt.Errorf("variant %s compatibility input-set mapper: %w", reductions[reductionIndex].name, err)
		}
		inputSetMappers[reductionIndex] = mapper
	}
	resolve = func(reductionIndex, nodeIndex int) (string, error) {
		if reductionIndex < 0 || reductionIndex >= len(records) ||
			nodeIndex < 0 || nodeIndex >= len(records[reductionIndex]) {
			return "", fmt.Errorf("family structural compatibility references unknown node [%d,%d]", reductionIndex, nodeIndex)
		}
		if id := resolved[reductionIndex][nodeIndex]; id != "" {
			return id, nil
		}
		record := records[reductionIndex][nodeIndex]
		if stack[reductionIndex][nodeIndex] {
			return "", fmt.Errorf("variant %s compatibility graph contains a cycle at %s", reductions[record.reductionIndex].name, record.nodeID)
		}
		stack[reductionIndex][nodeIndex] = true
		node := reductions[record.reductionIndex].plan.Nodes[nodeIndex]
		producerIDs := make([]string, len(node.Inputs))
		defaultClass := record.fullConfigID == ""
		for index, input := range node.Inputs {
			producerIndex, ok := nodeOrdinals[record.reductionIndex][input.ProducerID]
			if !ok {
				return "", fmt.Errorf("variant %s node %s compatibility input references unknown producer %s", reductions[record.reductionIndex].name, record.nodeID, input.ProducerID)
			}
			producerID, err := resolve(record.reductionIndex, producerIndex)
			if err != nil {
				return "", err
			}
			producerIDs[index] = producerID
			producerBaseID := records[record.reductionIndex][producerIndex].baseID
			if producerID != producerBaseID {
				defaultClass = false
			}
		}
		inputSetID, err := inputSetMappers[record.reductionIndex].Map(node.InputSet)
		if err != nil {
			return "", err
		}
		if inputSetID != reductions[record.reductionIndex].structuralInputSetRoots[node.ID] {
			defaultClass = false
		}
		stack[reductionIndex][nodeIndex] = false
		id := record.baseID
		if !defaultClass {
			id = derivedFamilyStructuralCompatibilityID(record.baseID, record.fullConfigID, node, producerIDs, inputSetID)
		}
		resolved[reductionIndex][nodeIndex] = id
		return id, nil
	}

	for reductionIndex := range reductions {
		for nodeIndex := range reductions[reductionIndex].plan.Nodes {
			if _, err := resolve(reductionIndex, nodeIndex); err != nil {
				return nil, err
			}
		}
	}
	groupSets := map[string][]ConfigDependencySet{}
	for reductionIndex := range reductions {
		for nodeIndex, node := range reductions[reductionIndex].plan.Nodes {
			id := resolved[reductionIndex][nodeIndex]
			reductions[reductionIndex].structuralID[node.ID] = id
			groupSets[id] = append(groupSets[id], records[reductionIndex][nodeIndex].dependencies)
		}
	}
	unionSets := make(map[string]ConfigDependencySet, len(groupSets))
	for groupID, sets := range groupSets {
		set, err := UnionConfigDependencySets(sets...)
		if err != nil {
			return nil, fmt.Errorf("structural compatibility group %s config dependencies: %w", groupID, err)
		}
		unionSets[groupID] = set
	}
	return unionSets, nil
}

func familyNodeIDsWithArtifactPath(
	plan *ActionPlan,
	sourceIdentity func(ActionPlanSource) string,
	artifactPathIdentity func(ActionPlanOutput) string,
) (map[string]string, map[string][]byte, map[string]string, error) {
	return familyNodeIDsWithOutputIdentity(
		plan, sourceIdentity, func(output ActionPlanOutput) string { return output.Path }, artifactPathIdentity,
	)
}

func familyNodeIDsWithOutputIdentity(
	plan *ActionPlan,
	sourceIdentity func(ActionPlanSource) string,
	outputPathIdentity func(ActionPlanOutput) string,
	artifactPathIdentity func(ActionPlanOutput) string,
) (map[string]string, map[string][]byte, map[string]string, error) {
	sources := make(map[string]ActionPlanSource, len(plan.Sources))
	for _, source := range plan.Sources {
		sources[source.ID] = source
	}
	nodes := make(map[string]ActionPlanNode, len(plan.Nodes))
	for _, node := range plan.Nodes {
		nodes[node.ID] = node
	}
	inputSets, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return nil, nil, nil, err
	}
	resolved := map[string]string{}
	payloads := map[string][]byte{}
	inputSetRoots := map[string]string{}
	stack := map[string]bool{}
	var resolve func(string) (string, error)
	inputSetMapper, err := inputSets.NewTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		if entry.SourceID != "" {
			source, ok := sources[entry.SourceID]
			if !ok {
				return ActionPlanInputSetEntry{}, fmt.Errorf("family input set references unknown source %s", entry.SourceID)
			}
			identity := sourceIdentity(source)
			if !validFamilySourceID(identity) {
				identity = semanticFamilySourceID("input-set-structural", identity)
			}
			entry.SourceID = identity
			return entry, nil
		}
		producer, err := resolve(entry.ProducerID)
		if err != nil {
			return ActionPlanInputSetEntry{}, err
		}
		entry.ProducerID = producer
		return entry, nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	resolve = func(oldID string) (string, error) {
		if id := resolved[oldID]; id != "" {
			return id, nil
		}
		node, ok := nodes[oldID]
		if !ok {
			return "", fmt.Errorf("family node references unknown producer %s", oldID)
		}
		if stack[oldID] {
			return "", fmt.Errorf("family action plan contains a cycle at %s", oldID)
		}
		stack[oldID] = true
		sourceIDs := make([]string, len(node.Sources))
		for index, edge := range node.Sources {
			source, ok := sources[edge.SourceID]
			if !ok {
				return "", fmt.Errorf("family node %s references unknown source %s", oldID, edge.SourceID)
			}
			sourceIDs[index] = sourceIdentity(source)
		}
		producerIDs := make([]string, len(node.Inputs))
		for index, edge := range node.Inputs {
			producer, err := resolve(edge.ProducerID)
			if err != nil {
				return "", err
			}
			producerIDs[index] = producer
		}
		inputSetID, err := inputSetMapper.Map(node.InputSet)
		if err != nil {
			return "", err
		}
		delete(stack, oldID)
		payload := semanticFamilyNodeBytes(node, sourceIDs, producerIDs, inputSetID, outputPathIdentity, artifactPathIdentity)
		digest := sha256.Sum256(payload)
		id := hex.EncodeToString(digest[:])
		resolved[oldID] = id
		payloads[oldID] = payload
		inputSetRoots[oldID] = inputSetID
		return id, nil
	}
	for _, node := range plan.Nodes {
		if _, err := resolve(node.ID); err != nil {
			return nil, nil, nil, err
		}
	}
	return resolved, payloads, inputSetRoots, nil
}

func familyNodeIDs(plan *ActionPlan, sourceIdentity func(ActionPlanSource) string) (map[string]string, map[string][]byte, map[string]string, error) {
	return familyNodeIDsWithArtifactPath(plan, sourceIdentity, actionPlanOutputArtifactPath)
}

func mergeActionPlanFamilyInputSetNodes(
	destination map[string]ActionPlanInputSetNode,
	incoming map[string]ActionPlanInputSetNode,
) error {
	for id, node := range incoming {
		if previous, ok := destination[id]; ok {
			previousBytes, previousErr := actionPlanInputSetCanonicalNode(previous)
			nodeBytes, nodeErr := actionPlanInputSetCanonicalNode(node)
			if previousErr != nil || nodeErr != nil || !bytes.Equal(previousBytes, nodeBytes) {
				return fmt.Errorf("input-set content ID collision %s", id)
			}
			continue
		}
		destination[id] = cloneActionPlanInputSetNode(node)
	}
	return nil
}

func fullConfigSourceIdentity(source ActionPlanSource) string {
	if source.Namespace == "config" {
		// Structural matching deliberately erases config values and projection
		// ordinals. A symmetric dependency set and capsule are attached below
		// before the final identity is computed.
		return "config-projection:" + source.Path
	}
	return semanticFamilySourceID(source.Namespace, source.Path)
}

// familyConfigCapsuleCacheKey retains exactly the dependency fields consumed
// by RenderConfigCapsule. UnionConfigDependencySets has already canonicalized
// Symbols before this is called. Opaque capsules copy every resolved config
// byte regardless of their diagnostic reason or source/object evidence.
func familyConfigCapsuleCacheKey(dependencies ConfigDependencySet) string {
	if dependencies.Opaque {
		return "opaque"
	}
	return "symbols\x00" + strings.Join(dependencies.Symbols, "\x00")
}

func configSourceCapsulePath(sourcePath string) (string, bool) {
	for _, projection := range resolvedConfigProjections() {
		if projection.input == sourcePath {
			return projection.output, true
		}
	}
	return "", false
}

func familyConfigSourceUsage(plan *ActionPlan, sources map[string]ActionPlanSource) (map[string]bool, error) {
	if plan == nil {
		return nil, fmt.Errorf("family config-source index requires an action plan")
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	contains := map[string]bool{}
	var inputSetContainsConfig func(string) (bool, error)
	inputSetContainsConfig = func(inputSetID string) (bool, error) {
		if inputSetID == "" {
			return false, nil
		}
		if known[inputSetID] {
			return contains[inputSetID], nil
		}
		inputSetNode, ok := store.Node(inputSetID)
		if !ok {
			return false, fmt.Errorf("input-set node %s is missing", inputSetID)
		}
		hasConfig := false
		for _, entry := range inputSetNode.Entries {
			if entry.SourceID == "" {
				continue
			}
			source, ok := sources[entry.SourceID]
			if !ok {
				return false, fmt.Errorf("input set references unknown source %s", entry.SourceID)
			}
			if source.Namespace == "config" {
				hasConfig = true
				break
			}
		}
		if !hasConfig {
			for _, child := range inputSetNode.Children {
				childHasConfig, err := inputSetContainsConfig(child.ID)
				if err != nil {
					return false, err
				}
				if childHasConfig {
					hasConfig = true
					break
				}
			}
		}
		known[inputSetID] = true
		contains[inputSetID] = hasConfig
		return hasConfig, nil
	}

	usage := make(map[string]bool, len(plan.Nodes))
	for _, node := range plan.Nodes {
		for _, edge := range node.Sources {
			source, ok := sources[edge.SourceID]
			if !ok {
				return nil, fmt.Errorf("node %s references unknown source %s", node.ID, edge.SourceID)
			}
			if source.Namespace == "config" {
				usage[node.ID] = true
				break
			}
		}
		if usage[node.ID] {
			continue
		}
		hasConfig, err := inputSetContainsConfig(node.InputSet)
		if err != nil {
			return nil, fmt.Errorf("node %s input set: %w", node.ID, err)
		}
		usage[node.ID] = hasConfig
	}
	return usage, nil
}

func typedFamilyCompilerNode(plan *ActionPlan, node ActionPlanNode) bool {
	// Typed compound compiler actions deliberately execute through scriptrun so
	// the compiler, fixdep, and their private depfile share one working tree.
	// Their exact cc/cxx argv is carried by ActionRecipe.CompilerInvocation and
	// must remain distinguishable from arbitrary scriptrun actions. Only the
	// bounded cmd_and_fixdep recognizer emits CompilerInvocation after proving the
	// remaining ld/mv/objtool/fixdep/cleanup envelope.
	if plan == nil || node.Kind != "compile" {
		return false
	}
	if node.Tool == "cc" || node.Tool == "cxx" {
		return true
	}
	recipe, ok := plan.Recipes[node.Recipe]
	return ok && node.Tool == compactKbuildScriptRunnerRole && recipe.CompilerInvocation != nil &&
		(recipe.CompilerInvocation.Tool == "cc" || recipe.CompilerInvocation.Tool == "cxx")
}

func preciseFamilyCompilerNode(plan *ActionPlan, node ActionPlanNode, dependencies ConfigDependencySet) bool {
	// Precision is deliberately a second dimension. TypedCompilerNodes reports
	// the complete cc/cxx population, while this predicate identifies the subset
	// for which dependency analysis proved an exact source/object/config closure.
	return !dependencies.Opaque && typedFamilyCompilerNode(plan, node)
}

func knownConfigFreeFamilyNode(plan *ActionPlan, node ActionPlanNode) bool {
	if plan == nil {
		return false
	}
	recipe, ok := plan.Recipes[node.Recipe]
	if !ok {
		return false
	}
	// Untyped driver links may compile source operands and include config;
	// preserve the full capsule even when supplied an empty annotation.
	return node.Kind == "archive" || node.Kind == "link-relocatable" || recipe.Tool == "as"
}

func familyRecipeSemanticBindings(recipe ActionRecipe) map[string]bool {
	bindings := map[string]bool{}
	visit := func(value string) {
		for _, match := range actionRecipePlaceholder.FindAllStringSubmatch(value, -1) {
			if match[1] == "source" || match[1] == "input" {
				bindings[match[1]+":"+match[2]] = true
			}
		}
	}
	for _, argument := range recipe.Arguments {
		visit(argument)
	}
	visit(recipe.WorkingDirectory)
	for _, value := range recipe.Environment {
		visit(value)
	}
	visit(recipe.Stdin)
	visit(recipe.Stdout)
	if recipe.CompilerInvocation != nil {
		for _, argument := range recipe.CompilerInvocation.Arguments {
			visit(argument)
		}
	}
	for _, substitution := range recipe.ContentSubstitutions {
		if strings.HasPrefix(substitution.Input, "source:") || strings.HasPrefix(substitution.Input, "input:") {
			bindings[substitution.Input] = true
		}
	}
	for _, replay := range recipe.CommandReplays {
		for _, invocation := range replay.Invocations {
			for _, argument := range invocation.Arguments {
				visit(argument)
			}
			for _, output := range invocation.Outputs {
				visit(output)
			}
		}
	}
	if binding, ok := strings.CutPrefix(recipe.Tool, "input:"); ok {
		bindings["input:"+binding] = true
	}
	for _, value := range []string{recipe.Stdin, recipe.Stdout} {
		if strings.HasPrefix(value, "source:") || strings.HasPrefix(value, "input:") {
			bindings[value] = true
		}
	}
	for _, binding := range recipe.ExecutableInputs {
		bindings["input:"+binding] = true
	}
	for _, bases := range recipe.ObservedOutputBases {
		for _, binding := range bases {
			bindings["input:"+binding] = true
		}
	}
	return bindings
}

func familyRecipeSemanticWorkingPaths(recipe ActionRecipe) map[string]bool {
	paths := make(map[string]bool, len(recipe.WorkingOutputs)+len(recipe.ObservedOutputs))
	for _, pathname := range recipe.WorkingOutputs {
		paths[pathname] = true
	}
	for _, pathname := range recipe.ObservedOutputs {
		paths[pathname] = true
	}
	return paths
}

func rewriteFamilyRecipeBindingValue(value string, remap map[string]string) string {
	return actionRecipePlaceholder.ReplaceAllStringFunc(value, func(placeholder string) string {
		match := actionRecipePlaceholder.FindStringSubmatch(placeholder)
		if len(match) != 3 || match[1] != "source" && match[1] != "input" {
			return placeholder
		}
		if binding := remap[match[1]+":"+match[2]]; binding != "" {
			return "${" + match[1] + ":" + binding + "}"
		}
		return placeholder
	})
}

func rewriteFamilyRecipeBindings(recipe ActionRecipe, remap map[string]string, retainedWorkingInputs map[string]string) ActionRecipe {
	recipe = cloneActionRecipe(recipe)
	for index := range recipe.Arguments {
		recipe.Arguments[index] = rewriteFamilyRecipeBindingValue(recipe.Arguments[index], remap)
	}
	recipe.WorkingDirectory = rewriteFamilyRecipeBindingValue(recipe.WorkingDirectory, remap)
	for name, value := range recipe.Environment {
		recipe.Environment[name] = rewriteFamilyRecipeBindingValue(value, remap)
	}
	recipe.Stdin = rewriteFamilyRecipeBindingValue(recipe.Stdin, remap)
	recipe.Stdout = rewriteFamilyRecipeBindingValue(recipe.Stdout, remap)
	if binding := remap[recipe.Stdin]; binding != "" && (strings.HasPrefix(recipe.Stdin, "source:") || strings.HasPrefix(recipe.Stdin, "input:")) {
		kind, _, _ := strings.Cut(recipe.Stdin, ":")
		recipe.Stdin = kind + ":" + binding
	}
	if binding := remap[recipe.Stdout]; binding != "" && (strings.HasPrefix(recipe.Stdout, "source:") || strings.HasPrefix(recipe.Stdout, "input:")) {
		kind, _, _ := strings.Cut(recipe.Stdout, ":")
		recipe.Stdout = kind + ":" + binding
	}
	if recipe.CompilerInvocation != nil {
		for index := range recipe.CompilerInvocation.Arguments {
			recipe.CompilerInvocation.Arguments[index] = rewriteFamilyRecipeBindingValue(recipe.CompilerInvocation.Arguments[index], remap)
		}
		for index, reference := range recipe.CompilerInvocation.WorkingInputUses {
			if binding := remap[reference]; binding != "" {
				kind, _, _ := strings.Cut(reference, ":")
				recipe.CompilerInvocation.WorkingInputUses[index] = kind + ":" + binding
			}
		}
		sort.Strings(recipe.CompilerInvocation.WorkingInputUses)
		for index, reference := range recipe.CompilerInvocation.AuxiliaryWorkingInputUses {
			if binding := remap[reference]; binding != "" {
				kind, _, _ := strings.Cut(reference, ":")
				recipe.CompilerInvocation.AuxiliaryWorkingInputUses[index] = kind + ":" + binding
			}
		}
		sort.Strings(recipe.CompilerInvocation.AuxiliaryWorkingInputUses)
	}
	for name, substitution := range recipe.ContentSubstitutions {
		if binding := remap[substitution.Input]; binding != "" {
			kind, _, _ := strings.Cut(substitution.Input, ":")
			substitution.Input = kind + ":" + binding
			recipe.ContentSubstitutions[name] = substitution
		}
	}
	for replayIndex := range recipe.CommandReplays {
		for invocationIndex := range recipe.CommandReplays[replayIndex].Invocations {
			invocation := &recipe.CommandReplays[replayIndex].Invocations[invocationIndex]
			for index := range invocation.Arguments {
				invocation.Arguments[index] = rewriteFamilyRecipeBindingValue(invocation.Arguments[index], remap)
			}
			for index := range invocation.Outputs {
				invocation.Outputs[index] = rewriteFamilyRecipeBindingValue(invocation.Outputs[index], remap)
			}
		}
	}
	if binding, ok := strings.CutPrefix(recipe.Tool, "input:"); ok {
		if renamed := remap["input:"+binding]; renamed != "" {
			recipe.Tool = "input:" + renamed
		}
	}
	for index, binding := range recipe.ExecutableInputs {
		if renamed := remap["input:"+binding]; renamed != "" {
			recipe.ExecutableInputs[index] = renamed
		}
	}
	for output, bases := range recipe.ObservedOutputBases {
		for index, binding := range bases {
			if renamed := remap["input:"+binding]; renamed != "" {
				bases[index] = renamed
			}
		}
		recipe.ObservedOutputBases[output] = bases
	}
	recipe.WorkingInputs = retainedWorkingInputs
	return recipe
}

// familyCompilerInputCanReattachFromPriorTree recognizes an explicit compiler
// input whose only semantic purpose is to materialize one proven ObjectPaths
// member.  Removing that variant-local edge before structural matching is safe
// only when addFamilyPriorTreeInputs can reconstruct the same file from the
// union closure: the producer is in a strict-prior stage, owns a canonical path
// in a declared tree, and an explicit working-input copy (when present) is
// exactly equivalent to copying that private tree into the working root.
func familyCompilerInputCanReattachFromPriorTree(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
	inputIndex int,
	edge ActionPlanNodeEdge,
	objectClosure map[string]bool,
) bool {
	producer, exists := plan.nodesByID[edge.ProducerID]
	if !exists || edge.Slot < 0 || edge.Slot >= len(producer.Outputs) ||
		familyStageOrdinal(producer.Stage) >= familyStageOrdinal(node.Stage) ||
		projectedGeneratorOutputIsInternal(plan, producer.ID, edge.Slot) {
		return false
	}
	output := producer.Outputs[edge.Slot]
	outputPath := canonicalKbuildRulePath(output.Path)
	if !actionPlanOutputIsCanonical(output) || !objectClosure[outputPath] ||
		!slices.Contains(node.Trees, output.Tree) || !slices.Contains(recipe.Trees, output.Tree) {
		return false
	}
	binding := edge.Role + ":" + planOrdinal(inputIndex)
	workingPath, staged := recipe.WorkingInputs["input:"+binding]
	if !staged {
		return true
	}
	return canonicalKbuildRulePath(workingPath) == outputPath && slices.Contains(recipe.WorkingTrees, output.Tree)
}

// familyPreciseInputSetPruner shares the consumer-independent work across
// persistent roots. The baseline retains complete compound uses (or unknown
// target forms); consumer-specific closure/working paths are restored through
// exact radix lookups. Never flatten a cumulative root for each compiler.
type familyPreciseInputSetPruner struct {
	plan     *ActionPlan
	store    *ActionPlanInputSetStore
	indexed  map[string]bool
	aliases  map[string]map[ActionPlanInputSetTarget]bool
	filtered [2]map[string]string
}

func (p *familyPreciseInputSetPruner) index(root string) error {
	if root == "" || p.indexed[root] {
		return nil
	}
	node, err := p.store.nodeAt(root)
	if err != nil {
		return err
	}
	for _, entry := range node.Entries {
		if entry.Target.Kind != ActionPlanInputSetWorkTarget {
			continue
		}
		alias := func(pathname string) {
			if pathname == "" || pathname == entry.Target.Path {
				return
			}
			if p.aliases[pathname] == nil {
				p.aliases[pathname] = map[ActionPlanInputSetTarget]bool{}
			}
			p.aliases[pathname][entry.Target] = true
		}
		if entry.SourceID != "" {
			source, ok := p.plan.sourcesByID[entry.SourceID]
			if !ok {
				return fmt.Errorf("input set references unknown source %s", entry.SourceID)
			}
			alias(source.Path)
			if source.Namespace == "config" {
				projection, _ := configSourceCapsulePath(source.Path)
				alias(projection)
			}
		} else {
			producer, ok := p.plan.nodesByID[entry.ProducerID]
			if !ok || entry.Slot < 0 || entry.Slot >= len(producer.Outputs) {
				return fmt.Errorf("input set references unavailable producer output %s[%d]", entry.ProducerID, entry.Slot)
			}
			alias(producer.Outputs[entry.Slot].Path)
		}
	}
	for _, child := range node.Children {
		if err := p.index(child.ID); err != nil {
			return err
		}
	}
	p.indexed[root] = true
	return nil
}

func (p *familyPreciseInputSetPruner) baseline(root string, mode int) (string, error) {
	if root == "" {
		return "", nil
	}
	if result, ok := p.filtered[mode][root]; ok {
		return result, nil
	}
	node, err := p.store.nodeAt(root)
	if err != nil {
		return "", err
	}
	result := root
	if len(node.Entries) != 0 {
		entries := make([]ActionPlanInputSetEntry, 0, len(node.Entries))
		for _, entry := range node.Entries {
			if entry.Target.Kind != ActionPlanInputSetWorkTarget || mode == 1 && entry.CompilerUse {
				entries = append(entries, entry)
			}
		}
		if len(entries) != len(node.Entries) {
			result, err = p.store.build(node.Depth, entries)
		}
	} else {
		children := make([]ActionPlanInputSetChild, 0, len(node.Children))
		changed := false
		for _, child := range node.Children {
			id, childErr := p.baseline(child.ID, mode)
			if childErr != nil {
				return "", childErr
			}
			changed = changed || id != child.ID
			if id != "" {
				children = append(children, ActionPlanInputSetChild{Nibble: child.Nibble, ID: id})
			}
		}
		if changed {
			count, countErr := p.store.childCount(children)
			if countErr != nil {
				return "", countErr
			}
			if count <= actionPlanInputSetLeafCapacity {
				entries, collectErr := p.store.collectChildren(children, count)
				if collectErr != nil {
					return "", collectErr
				}
				result, err = p.store.build(node.Depth, entries)
			} else {
				result, err = p.store.internBranch(node.Depth, children)
			}
		}
	}
	if err != nil {
		return "", err
	}
	p.filtered[mode][root] = result
	return result, nil
}

func (p *familyPreciseInputSetPruner) prune(
	node ActionPlanNode,
	recipe ActionRecipe,
	objectClosure, semanticWorkingPaths map[string]bool,
) (string, error) {
	if node.InputSet == "" || recipe.CompilerInvocation != nil && !recipe.CompilerInvocation.WorkingInputUsesComplete {
		return node.InputSet, nil
	}
	if p.store == nil {
		store, err := p.plan.planningActionPlanInputSetStore()
		if err != nil {
			return "", err
		}
		p.store = store
		p.indexed = map[string]bool{}
		p.aliases = map[string]map[ActionPlanInputSetTarget]bool{}
		p.filtered = [2]map[string]string{{}, {}}
	}
	if err := p.index(node.InputSet); err != nil {
		return "", err
	}
	mode := 0
	if recipe.CompilerInvocation != nil {
		mode = 1
	}
	root, err := p.baseline(node.InputSet, mode)
	if err != nil {
		return "", err
	}
	paths := maps.Clone(objectClosure)
	for pathname := range semanticWorkingPaths {
		paths[pathname] = true
	}
	targets := map[ActionPlanInputSetTarget]bool{}
	for pathname := range paths {
		if pathname == "" {
			continue
		}
		targets[ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: pathname}] = true
		for target := range p.aliases[pathname] {
			targets[target] = true
		}
	}
	for target := range targets {
		entry, found, err := p.store.Lookup(node.InputSet, target)
		if err != nil {
			return "", err
		}
		if !found {
			continue
		}
		envelopeRequired := mode == 1 && entry.CompilerUse
		semanticRequired := semanticWorkingPaths[entry.Target.Path]
		closureRequired := objectClosure[entry.Target.Path]
		if entry.SourceID != "" {
			source := p.plan.sourcesByID[entry.SourceID]
			closureRequired = closureRequired || objectClosure[source.Path]
			if source.Namespace == "config" {
				projection, _ := configSourceCapsulePath(source.Path)
				closureRequired = closureRequired || objectClosure[projection]
			}
		} else {
			producer := p.plan.nodesByID[entry.ProducerID]
			output := producer.Outputs[entry.Slot]
			closureRequired = closureRequired || objectClosure[output.Path]
			// This is the persistent equivalent of
			// familyCompilerInputCanReattachFromPriorTree: only a canonical,
			// strict-prior tree projection can be rebuilt from the union closure.
			if closureRequired && !envelopeRequired && !semanticRequired &&
				familyStageOrdinal(producer.Stage) < familyStageOrdinal(node.Stage) &&
				!projectedGeneratorOutputIsInternal(p.plan, producer.ID, entry.Slot) &&
				actionPlanOutputIsCanonical(output) && objectClosure[output.Path] && entry.Target.Path == output.Path &&
				slices.Contains(node.Trees, output.Tree) && slices.Contains(recipe.Trees, output.Tree) &&
				slices.Contains(recipe.WorkingTrees, output.Tree) {
				continue
			}
		}
		if !envelopeRequired && !semanticRequired && !closureRequired {
			continue
		}
		root, err = p.store.Insert(root, entry)
		if err != nil {
			return "", err
		}
	}
	return root, nil
}

func prunePreciseFamilyCompilerInputs(plan *ActionPlan, node *ActionPlanNode, dependencies ConfigDependencySet) (ActionRecipe, bool, error) {
	return prunePreciseFamilyCompilerInputsWithInputSets(plan, node, dependencies, &familyPreciseInputSetPruner{plan: plan})
}

func prunePreciseFamilyCompilerInputsWithInputSets(
	plan *ActionPlan,
	node *ActionPlanNode,
	dependencies ConfigDependencySet,
	inputSets *familyPreciseInputSetPruner,
) (ActionRecipe, bool, error) {
	if plan == nil || node == nil {
		return ActionRecipe{}, false, fmt.Errorf("precise compiler input pruning requires an action plan and node")
	}
	recipe, ok := plan.Recipes[node.Recipe]
	if !ok {
		return ActionRecipe{}, false, fmt.Errorf("node %s references unknown recipe %s", node.ID, node.Recipe)
	}
	if !preciseFamilyCompilerNode(plan, *node, dependencies) {
		return recipe, false, nil
	}
	if err := plan.ensureSourceLookupIndex(); err != nil {
		return ActionRecipe{}, false, err
	}
	plan.ensureNodeLookupIndexes()
	objectClosure := make(map[string]bool, len(dependencies.ObjectPaths))
	for _, pathname := range dependencies.ObjectPaths {
		objectClosure[pathname] = true
	}
	// A typed compound compile executes its compiler, fixdep, and other bounded
	// commands from one base64-encoded outer script. CompilerInvocation exposes
	// only the compiler argv to config-dependency analysis; it cannot prove that
	// a staged binding is irrelevant to a middle ld/objtool command. Retain the
	// complete staged frontier for that compound envelope. Direct cc/cxx recipes
	// remain eligible for exact preprocessing-closure pruning below.
	compoundCompiler := recipe.CompilerInvocation != nil
	completeCompoundUses := compoundCompiler && recipe.CompilerInvocation.WorkingInputUsesComplete
	compoundUses := map[string]bool{}
	if completeCompoundUses {
		for _, reference := range recipe.CompilerInvocation.WorkingInputUses {
			compoundUses[reference] = true
		}
	}
	semantic := familyRecipeSemanticBindings(recipe)
	semanticWorkingPaths := familyRecipeSemanticWorkingPaths(recipe)
	inputSet, err := inputSets.prune(*node, recipe, objectClosure, semanticWorkingPaths)
	if err != nil {
		return ActionRecipe{}, false, fmt.Errorf("node %s precise input set: %w", node.ID, err)
	}
	inputSetChanged := inputSet != node.InputSet
	remap := map[string]string{}
	retainedWorkingInputs := map[string]string{}
	retainedSources := make([]ActionPlanSourceEdge, 0, len(node.Sources))
	for index, edge := range node.Sources {
		oldBinding := edge.Role + ":" + planOrdinal(index)
		oldReference := "source:" + oldBinding
		workingPath, staged := recipe.WorkingInputs[oldReference]
		workingPath = canonicalKbuildRulePath(workingPath)
		source := plan.sourcesByID[edge.SourceID]
		projection := ""
		if source.Namespace == "config" {
			projection, _ = configSourceCapsulePath(source.Path)
		}
		keep := compoundCompiler && staged && (!completeCompoundUses || compoundUses[oldReference]) || semantic[oldReference] || semanticWorkingPaths[recipe.WorkingInputs[oldReference]] ||
			objectClosure[source.Path] || objectClosure[workingPath] || objectClosure[projection]
		if !keep {
			continue
		}
		newBinding := edge.Role + ":" + planOrdinal(len(retainedSources))
		remap[oldReference] = newBinding
		if staged {
			retainedWorkingInputs["source:"+newBinding] = recipe.WorkingInputs[oldReference]
		}
		retainedSources = append(retainedSources, edge)
	}
	retainedInputs := make([]ActionPlanNodeEdge, 0, len(node.Inputs))
	for index, edge := range node.Inputs {
		oldBinding := edge.Role + ":" + planOrdinal(index)
		oldReference := "input:" + oldBinding
		workingPath, staged := recipe.WorkingInputs[oldReference]
		workingPath = canonicalKbuildRulePath(workingPath)
		producer, exists := plan.nodesByID[edge.ProducerID]
		if !exists || edge.Slot < 0 || edge.Slot >= len(producer.Outputs) {
			return ActionRecipe{}, false, fmt.Errorf("node %s references unavailable producer output %s[%d]", node.ID, edge.ProducerID, edge.Slot)
		}
		outputPath := producer.Outputs[edge.Slot].Path
		envelopeRequired := compoundCompiler && staged && (!completeCompoundUses || compoundUses[oldReference])
		semanticRequired := semantic[oldReference] || semanticWorkingPaths[recipe.WorkingInputs[oldReference]]
		closureRequired := objectClosure[outputPath] || objectClosure[workingPath]
		keep := envelopeRequired || semanticRequired || closureRequired || edge.Role == "sequence"
		// A strict-prior canonical tree input which is present only because this
		// variant's dependency scan observed it must not salt the structural ID.
		// The symmetric ObjectPaths union is installed by addFamilyPriorTreeInputs
		// before final semantic identities are computed. Inputs used by argv, an
		// outer compound envelope, same-stage ordering, or a noncanonical staging
		// contract remain explicit and variant-local.
		if closureRequired && !envelopeRequired && !semanticRequired && edge.Role != "sequence" &&
			familyCompilerInputCanReattachFromPriorTree(plan, *node, recipe, index, edge, objectClosure) {
			keep = false
		}
		if !keep {
			continue
		}
		newBinding := edge.Role + ":" + planOrdinal(len(retainedInputs))
		remap[oldReference] = newBinding
		if staged {
			retainedWorkingInputs["input:"+newBinding] = recipe.WorkingInputs[oldReference]
		}
		retainedInputs = append(retainedInputs, edge)
	}
	if len(retainedSources) == len(node.Sources) && len(retainedInputs) == len(node.Inputs) {
		node.InputSet = inputSet
		return recipe, inputSetChanged, nil
	}
	node.InputSet = inputSet
	node.Sources = retainedSources
	node.Inputs = retainedInputs
	recipe = rewriteFamilyRecipeBindings(recipe, remap, retainedWorkingInputs)
	recipe.Sources = make([]string, len(node.Sources))
	for index, edge := range node.Sources {
		recipe.Sources[index] = edge.Role + ":" + planOrdinal(index)
	}
	recipe.Inputs = make([]string, len(node.Inputs))
	for index, edge := range node.Inputs {
		recipe.Inputs[index] = edge.Role + ":" + planOrdinal(index)
	}
	return recipe, true, nil
}

func familySourceProjectionLess(left, right ActionPlanFamilySourceProjection) bool {
	if left.Tree != right.Tree {
		return left.Tree < right.Tree
	}
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	return left.SourceOrdinal < right.SourceOrdinal
}

// attachPreciseFamilySourceClosure installs the immutable source-tree closure
// only after the symmetric dependency union is known. Before structural
// matching, prunePreciseFamilyCompilerInputs retains source edges which are
// semantically named by the recipe but discards evaluation-local header
// staging. Every matched variant then receives the same union closure here.
//
// The returned recipes have exact source bindings for every closure member.
// Nodes which expose the logical kernel tree additionally carry private source
// projections; direct-source-only compiler recipes still receive the exact
// Bazel Files without acquiring a tree binding they never requested.
func attachPreciseFamilySourceClosure(
	variant string,
	original, localized *ActionPlan,
	structuralIDs map[string]string,
	unionSets map[string]ConfigDependencySet,
	registerSource func(string, string) (string, error),
) (map[string]ActionRecipe, error) {
	if original == nil || localized == nil {
		return nil, fmt.Errorf("family source closure requires original and localized plans")
	}
	if len(original.Nodes) != len(localized.Nodes) {
		return nil, fmt.Errorf("family source closure changed node cardinality")
	}
	if err := original.ensureSourceLookupIndex(); err != nil {
		return nil, err
	}

	// SourcePaths name the logical source-tree location. Preserve an explicitly
	// selected namespace when the standalone plan already owns that exact path;
	// otherwise the path belongs to the primary kernel source tree. Multiple
	// physical namespaces for one logical path would make the projection
	// ambiguous and is rejected instead of selecting by iteration order.
	sourcesByPath := map[string][]ActionPlanSource{}
	for _, source := range original.Sources {
		if source.Namespace == "config" || source.Namespace == "capsule" {
			continue
		}
		pathname := canonicalKbuildRulePath(source.Path)
		sourcesByPath[pathname] = append(sourcesByPath[pathname], source)
	}
	resolveSource := func(pathname string) (ActionPlanSource, error) {
		candidates := sourcesByPath[pathname]
		if len(candidates) == 0 {
			return ActionPlanSource{Namespace: "kernel", Path: pathname}, nil
		}
		selected := candidates[0]
		for _, candidate := range candidates[1:] {
			if candidate.Namespace != selected.Namespace || candidate.Path != selected.Path {
				return ActionPlanSource{}, fmt.Errorf(
					"variant %s source closure path %q has ambiguous namespaces %q and %q",
					variant, pathname, selected.Namespace, candidate.Namespace,
				)
			}
		}
		return selected, nil
	}

	changedRecipes := map[string]ActionRecipe{}
	for nodeIndex := range localized.Nodes {
		node := &localized.Nodes[nodeIndex]
		originalNode := original.Nodes[nodeIndex]
		if node.ID != originalNode.ID {
			return nil, fmt.Errorf("family source closure node %d changed identity from %s to %s", nodeIndex, originalNode.ID, node.ID)
		}
		structuralID := structuralIDs[originalNode.ID]
		dependencies, ok := unionSets[structuralID]
		if structuralID == "" || !ok {
			return nil, fmt.Errorf("family source closure node %s has no symmetric dependency set", originalNode.ID)
		}
		if !preciseFamilyCompilerNode(original, originalNode, dependencies) || len(dependencies.SourcePaths) == 0 {
			continue
		}
		recipe, ok := localized.Recipes[node.Recipe]
		if !ok {
			return nil, fmt.Errorf("family source closure node %s references unknown recipe %s", originalNode.ID, node.Recipe)
		}
		if len(recipe.Sources) != len(node.Sources) {
			return nil, fmt.Errorf("family source closure node %s has %d source edges for %d recipe bindings", originalNode.ID, len(node.Sources), len(recipe.Sources))
		}

		sourceOrdinals := map[string]int{}
		for ordinal, edge := range node.Sources {
			sourceOrdinals[edge.SourceID] = ordinal
		}
		projectsKernelTree := slices.Contains(node.Trees, "kernel") && slices.Contains(recipe.Trees, "kernel")
		for _, pathname := range dependencies.SourcePaths {
			pathname = canonicalKbuildRulePath(pathname)
			descriptor, err := resolveSource(pathname)
			if err != nil {
				return nil, err
			}
			sourceID, err := registerSource(descriptor.Namespace, descriptor.Path)
			if err != nil {
				return nil, err
			}
			ordinal, exists := sourceOrdinals[sourceID]
			if !exists {
				if len(node.Sources) > maximumActionPlanOrdinal {
					return nil, fmt.Errorf("family source closure node %s exhausts source ordinal space", originalNode.ID)
				}
				ordinal = len(node.Sources)
				node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "kernel-source", SourceID: sourceID})
				recipe.Sources = append(recipe.Sources, "kernel-source:"+planOrdinal(ordinal))
				sourceOrdinals[sourceID] = ordinal
			}
			if projectsKernelTree {
				node.familySourceProjections = append(node.familySourceProjections, ActionPlanFamilySourceProjection{
					SourceOrdinal: ordinal, Tree: "kernel", Path: pathname,
				})
			}
		}
		sort.Slice(node.familySourceProjections, func(i, j int) bool {
			return familySourceProjectionLess(node.familySourceProjections[i], node.familySourceProjections[j])
		})
		if len(recipe.Sources) == len(localized.Recipes[node.Recipe].Sources) {
			continue
		}
		recipeID, err := recipe.ID()
		if err != nil {
			return nil, fmt.Errorf("variant %s node %s source-closure recipe: %w", variant, originalNode.ID, err)
		}
		localized.Recipes[recipeID] = cloneActionRecipe(recipe)
		node.Recipe = recipeID
		changedRecipes[recipeID] = cloneActionRecipe(recipe)
	}
	return changedRecipes, nil
}

// canonicalFallbackConfigProjection recognizes only the unconditional copy
// emitted by appendMissingConfigProjections. Selected Kbuild writers and
// versioned/noncanonical outputs must retain their full configuration input.
func canonicalFallbackConfigProjection(plan *ActionPlan, sources map[string]ActionPlanSource, node ActionPlanNode) (string, bool) {
	if plan == nil || node.Stage != "prep" || node.Kind != "copy" || node.Tool != "actionfile" ||
		node.Product != "sdk" || len(node.Sources) != 1 || len(node.Inputs) != 0 || len(node.Outputs) != 1 {
		return "", false
	}
	output := node.Outputs[0]
	if output.Tree != "prep" || !actionPlanOutputIsCanonical(output) || output.ObservedPath != "" {
		return "", false
	}
	source, ok := sources[node.Sources[0].SourceID]
	if !ok || source.Namespace != "config" || node.Sources[0].Role != "input" {
		return "", false
	}
	projection, ok := configSourceCapsulePath(source.Path)
	if !ok || projection != output.Path {
		return "", false
	}
	recipe, ok := plan.Recipes[node.Recipe]
	if !ok || recipe.Schema != LinuxKernelPlanSchema || recipe.Kind != "copy" || recipe.Tool != "actionfile" ||
		!slices.Equal(recipe.Arguments, []string{"-input", "${source:input:00000000}", "-out", "${output:00000000}"}) ||
		!slices.Equal(recipe.Sources, []string{"input:00000000"}) || len(recipe.Inputs) != 0 ||
		!slices.Equal(recipe.Outputs, []string{"00000000"}) {
		return "", false
	}
	return projection, true
}

type familyPriorTreeInput struct {
	producerID   string
	structuralID string
	slot         int
	stage        int
	tree         string
	logicalPath  string
	artifactPath string
}

func lessFamilyPriorTreeInput(left, right familyPriorTreeInput) bool {
	if left.tree != right.tree {
		return left.tree < right.tree
	}
	if left.structuralID != right.structuralID {
		return left.structuralID < right.structuralID
	}
	return left.slot < right.slot
}

func equalFamilyPriorTreeInputIdentity(left, right familyPriorTreeInput) bool {
	return left.tree == right.tree && left.structuralID == right.structuralID && left.slot == right.slot
}

type familyPrivateTreeProjectionOwner struct {
	producerID string
	slot       int
}

// familyNodePrivateTreeProjectionPaths returns the exact destinations already
// materialized in one private WorkingTrees view by generated input edges. Those
// projections form the immutable tree baseline; a later WorkingInputs copy may
// intentionally replace a leaf, but family-added ambient state must not do so
// accidentally. Distinct producers for one destination would otherwise leave
// the winner dependent on projection order, so reject that graph before it is
// emitted.
func familyNodePrivateTreeProjectionPaths(
	node ActionPlanNode,
	recipe ActionRecipe,
	nodes map[string]ActionPlanNode,
	tree string,
) (map[string]familyPrivateTreeProjectionOwner, error) {
	paths := map[string]familyPrivateTreeProjectionOwner{}
	if !slices.Contains(recipe.WorkingTrees, tree) {
		return paths, nil
	}
	if len(recipe.Inputs) != len(node.Inputs) {
		return nil, fmt.Errorf(
			"family node %s has %d input edges for %d recipe bindings",
			node.ID, len(node.Inputs), len(recipe.Inputs),
		)
	}
	for ordinal, input := range node.Inputs {
		producer, ok := nodes[input.ProducerID]
		if !ok || input.Slot < 0 || input.Slot >= len(producer.Outputs) {
			return nil, fmt.Errorf(
				"family node %s input %d references unavailable producer output %s[%d]",
				node.ID, ordinal, input.ProducerID, input.Slot,
			)
		}
		output := producer.Outputs[input.Slot]
		if output.Tree != tree {
			continue
		}
		pathname := canonicalKbuildRulePath(actionPlanOutputArtifactPath(output))
		owner := familyPrivateTreeProjectionOwner{producerID: input.ProducerID, slot: input.Slot}
		if previous, exists := paths[pathname]; exists && previous != owner {
			return nil, fmt.Errorf(
				"family node %s private %s tree path %q has ambiguous producer inputs %s[%d] and %s[%d]",
				node.ID, tree, pathname,
				previous.producerID, previous.slot, owner.producerID, owner.slot,
			)
		}
		paths[pathname] = owner
	}
	return paths, nil
}

// addFamilyPriorTreeInputs makes every generated file visible through a
// completed logical tree an exact graph edge. A family store contains outputs
// from every variant below nodes/<content-id>/<slot>; handing that store to a
// WorkingTrees consumer would expose physical storage paths and unrelated
// variants. Ordinary input bindings already carry both the physical store path
// and its logical projection, so synthetic unused recipe inputs are the
// narrowest lossless representation of a private tree view.
//
// Only strict-prior-stage producers are implicit. Same-stage generated files
// have always been selected by explicit ActionPlanNode.Inputs; treating every
// same-stage output as visible would add future edges and can create cycles.
// A precise compiler receives only the object paths in the symmetric union
// closure. Every other consumer retains the complete prior-stage tree.
func addFamilyPriorTreeInputs(
	original, localized *ActionPlan,
	structuralIDs map[string]string,
	unionSets map[string]ConfigDependencySet,
) (map[string]ActionRecipe, error) {
	if original == nil || localized == nil {
		return nil, fmt.Errorf("family prior-tree input lowering requires original and localized plans")
	}
	if len(original.Nodes) != len(localized.Nodes) {
		return nil, fmt.Errorf("family prior-tree input lowering changed node cardinality")
	}
	originalNodes := make(map[string]ActionPlanNode, len(original.Nodes))
	outputsByTree := map[string][]familyPriorTreeInput{}
	outputsByTreePath := map[string]map[string][]familyPriorTreeInput{}
	for _, node := range original.Nodes {
		originalNodes[node.ID] = node
		structuralID := structuralIDs[node.ID]
		if structuralID == "" {
			return nil, fmt.Errorf("family prior-tree producer %s has no structural identity", node.ID)
		}
		for slot, output := range node.Outputs {
			// Raw outputs, the full target replay, and comparison stamps are
			// validation-private. Full replay side slots intentionally remain the
			// exact public owners of their original Kbuild descriptors.
			if projectedGeneratorOutputIsInternal(original, node.ID, slot) {
				continue
			}
			logicalPath := canonicalKbuildRulePath(output.Path)
			artifactPath := canonicalKbuildRulePath(actionPlanOutputArtifactPath(output))
			candidate := familyPriorTreeInput{
				producerID: node.ID, structuralID: structuralID, slot: slot,
				stage: familyStageOrdinal(node.Stage), tree: output.Tree,
				logicalPath: logicalPath, artifactPath: artifactPath,
			}
			outputsByTree[output.Tree] = append(outputsByTree[output.Tree], candidate)
			paths := outputsByTreePath[output.Tree]
			if paths == nil {
				paths = map[string][]familyPriorTreeInput{}
				outputsByTreePath[output.Tree] = paths
			}
			paths[logicalPath] = append(paths[logicalPath], candidate)
			if artifactPath != logicalPath {
				paths[artifactPath] = append(paths[artifactPath], candidate)
			}
		}
	}
	// Candidate selection depends only on stage, named trees, precision, and
	// the canonical object closure. Thousands of compilers commonly share that
	// tuple; cache its sorted producer frontier and apply node-local explicit
	// edge filtering afterward.
	candidateCache := map[string][]familyPriorTreeInput{}
	changedRecipes := map[string]ActionRecipe{}
	for nodeIndex := range localized.Nodes {
		node := &localized.Nodes[nodeIndex]
		originalNode, ok := originalNodes[node.ID]
		if !ok {
			return nil, fmt.Errorf("localized family node %s has no original", node.ID)
		}
		if len(node.Trees) == 0 {
			continue
		}
		recipe, ok := localized.Recipes[node.Recipe]
		if !ok {
			return nil, fmt.Errorf("family node %s references unknown recipe %s", node.ID, node.Recipe)
		}
		if len(recipe.Inputs) != len(node.Inputs) {
			return nil, fmt.Errorf("family node %s has %d input edges for %d recipe bindings", node.ID, len(node.Inputs), len(recipe.Inputs))
		}
		dependencies, ok := unionSets[structuralIDs[node.ID]]
		if !ok {
			return nil, fmt.Errorf("family node %s has no symmetric config dependency set", node.ID)
		}
		precise := preciseFamilyCompilerNode(original, originalNode, dependencies)
		objectPaths := make(map[string]bool, len(dependencies.ObjectPaths))
		if precise {
			for _, pathname := range dependencies.ObjectPaths {
				objectPaths[canonicalKbuildRulePath(pathname)] = true
			}
		}
		type outputKey struct {
			producer string
			slot     int
		}
		type treePathKey struct {
			tree string
			path string
		}
		explicit := make(map[outputKey]bool, len(node.Inputs))
		explicitLogicalProjections := map[treePathKey]bool{}
		for ordinal, input := range node.Inputs {
			explicit[outputKey{producer: input.ProducerID, slot: input.Slot}] = true
			producer, exists := originalNodes[input.ProducerID]
			if !exists || input.Slot < 0 || input.Slot >= len(producer.Outputs) {
				return nil, fmt.Errorf("family node %s references unavailable producer output %s[%d]", node.ID, input.ProducerID, input.Slot)
			}
			binding := recipe.Inputs[ordinal]
			projection, staged := recipe.WorkingInputs["input:"+binding]
			if !staged {
				continue
			}
			output := producer.Outputs[input.Slot]
			explicitLogicalProjections[treePathKey{
				tree: output.Tree,
				path: canonicalKbuildRulePath(projection),
			}] = true
		}
		consumerStage := familyStageOrdinal(node.Stage)
		trees := slices.Clone(node.Trees)
		sort.Strings(trees)
		canonicalObjectPaths := slices.Sorted(maps.Keys(objectPaths))
		if precise {
			// A verified observation may have replaced an ordinary header's
			// producer with an immutable source at the same private-tree path.
			// Do not reattach that producer through the ambient prior-tree view.
			// Exact radix lookups avoid flattening cumulative roots per compiler.
			if node.InputSet != "" {
				store, err := localized.planningActionPlanInputSetStore()
				if err != nil {
					return nil, err
				}
				if err := localized.ensureSourceLookupIndex(); err != nil {
					return nil, err
				}
				for _, tree := range trees {
					for _, pathname := range canonicalObjectPaths {
						entry, found, err := store.Lookup(node.InputSet, ActionPlanInputSetTarget{
							Kind: ActionPlanInputSetTreeTarget, Tree: tree, Path: pathname,
						})
						if err != nil {
							return nil, err
						}
						if found && localized.sourcesByID[entry.SourceID].Namespace == LinuxKernelObservedHeaderSourceNamespace {
							explicitLogicalProjections[treePathKey{tree: tree, path: pathname}] = true
						}
					}
				}
			}
			for _, tree := range trees {
				for _, pathname := range canonicalObjectPaths {
					if explicitLogicalProjections[treePathKey{tree: tree, path: pathname}] {
						continue
					}
					logicalCandidates := []familyPriorTreeInput{}
					canonicalCandidates := []familyPriorTreeInput{}
					for _, candidate := range outputsByTreePath[tree][pathname] {
						if candidate.stage >= consumerStage || candidate.logicalPath != pathname {
							continue
						}
						logicalCandidates = append(logicalCandidates, candidate)
						if candidate.artifactPath == candidate.logicalPath {
							canonicalCandidates = append(canonicalCandidates, candidate)
						}
					}
					if len(canonicalCandidates) > 1 {
						return nil, fmt.Errorf(
							"family precise node %s has %d canonical %s producers for %q",
							node.ID, len(canonicalCandidates), tree, pathname,
						)
					}
					if len(logicalCandidates) == 0 || len(canonicalCandidates) == 1 {
						continue
					}
					unbound := []string{}
					for _, candidate := range logicalCandidates {
						unbound = append(unbound, candidate.producerID+"["+strconv.Itoa(candidate.slot)+"]")
					}
					sort.Strings(unbound)
					return nil, fmt.Errorf(
						"family precise node %s reads %s path %q with no canonical prior-stage producer; noncanonical candidates: %s",
						node.ID, tree, pathname, strings.Join(unbound, ", "),
					)
				}
			}
		}
		cacheKey := strconv.Itoa(consumerStage) + "\x00" + strconv.FormatBool(precise) + "\x00" +
			strings.Join(trees, "\x00") + "\x01" + strings.Join(canonicalObjectPaths, "\x00")
		candidates, cached := candidateCache[cacheKey]
		if !cached {
			if precise {
				for _, tree := range trees {
					for _, pathname := range canonicalObjectPaths {
						matches := outputsByTreePath[tree][pathname]
						logicalMatch := false
						for _, candidate := range matches {
							if candidate.stage >= consumerStage || candidate.logicalPath != pathname {
								continue
							}
							logicalMatch = true
							if candidate.artifactPath == candidate.logicalPath {
								candidates = append(candidates, candidate)
							}
						}
						if logicalMatch {
							continue
						}
						// A dependency scanner may name a private artifact path
						// explicitly. Preserve that exact versioned projection when
						// it is not also a Kbuild-visible logical pathname.
						for _, candidate := range matches {
							if candidate.stage < consumerStage && candidate.artifactPath == pathname {
								candidates = append(candidates, candidate)
							}
						}
					}
				}
			} else {
				for _, tree := range trees {
					for _, candidate := range outputsByTree[tree] {
						if candidate.stage < consumerStage {
							candidates = append(candidates, candidate)
						}
					}
				}
			}
			sort.Slice(candidates, func(i, j int) bool {
				return lessFamilyPriorTreeInput(candidates[i], candidates[j])
			})
			candidateCache[cacheKey] = candidates
		}
		if len(candidates) == 0 {
			continue
		}
		var previous familyPriorTreeInput
		havePrevious := false
		for _, candidate := range candidates {
			if explicitLogicalProjections[treePathKey{tree: candidate.tree, path: candidate.logicalPath}] {
				continue
			}
			if explicit[outputKey{producer: candidate.producerID, slot: candidate.slot}] {
				continue
			}
			if havePrevious && equalFamilyPriorTreeInputIdentity(candidate, previous) {
				continue
			}
			previous = candidate
			havePrevious = true
			role := "tree-" + candidate.tree
			ordinal := len(node.Inputs)
			node.Inputs = append(node.Inputs, ActionPlanNodeEdge{
				Role: role, ProducerID: candidate.producerID, Slot: candidate.slot,
			})
			recipe.Inputs = append(recipe.Inputs, role+":"+planOrdinal(ordinal))
		}
		recipeID, err := recipe.ID()
		if err != nil {
			return nil, fmt.Errorf("family node %s prior-tree recipe: %w", node.ID, err)
		}
		localized.Recipes[recipeID] = cloneActionRecipe(recipe)
		node.Recipe = recipeID
		changedRecipes[recipeID] = cloneActionRecipe(recipe)
	}
	return changedRecipes, nil
}

// validatedActionPlanFamily seals the interval between construction and
// emission. ActionPlanFamily deliberately exposes its representation for
// callers which need to inspect it, so a validation bit on that public value
// would become stale as soon as a caller mutated one of its maps or slices.
// This wrapper is instead created and consumed without letting the family
// escape in between.
type validatedActionPlanFamily struct {
	family *ActionPlanFamily
}

type actionPlanFamilyBuildVariant struct {
	variant           ActionPlanFamilyVariant
	snapshotValidated bool
	// These fields are only populated by the in-process verified replay
	// writer. Snapshot transports and public diagnostic uses cannot set them.
	observedHeaderUses []ConfigDependencyObservedHeaderUse
	observedHeaders    *ActionPlanFamilyObservedHeaders
	executionCut       *ActionPlanFamilyExecutionCut
}

// buildValidatedActionPlanFamily performs a symmetric reduction. It first
// matches the config-independent transitive graph, partitions those matches by
// compatible precision/full-config execution identity, unions conservative
// dependency sets inside each subgroup, renders one capsule per
// variant/member, and only then computes the final transitive identities.
func buildValidatedActionPlanFamily(variants []actionPlanFamilyBuildVariant) (*validatedActionPlanFamily, error) {
	return buildValidatedActionPlanFamilyWithStats(variants, nil)
}

func buildValidatedActionPlanFamilyWithStats(
	variants []actionPlanFamilyBuildVariant,
	validationStats *actionPlanFamilyValidationStats,
) (*validatedActionPlanFamily, error) {
	if len(variants) == 0 {
		return nil, fmt.Errorf("action plan family requires at least one variant")
	}
	ordered := slices.Clone(variants)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].variant.Name < ordered[j].variant.Name
	})
	reductions := make([]familyVariantReduction, 0, len(ordered))
	seenVariants := map[string]bool{}
	executionCut := ordered[0].executionCut
	for _, input := range ordered {
		if input.executionCut != executionCut {
			return nil, fmt.Errorf("family variants disagree on authenticated execution retention")
		}
		variant := input.variant
		if err := validatePlanName("family variant", variant.Name); err != nil {
			return nil, err
		}
		if seenVariants[variant.Name] {
			return nil, fmt.Errorf("action plan family repeats variant %q", variant.Name)
		}
		seenVariants[variant.Name] = true
		if !input.snapshotValidated {
			if err := variant.Snapshot.validate(); err != nil {
				return nil, fmt.Errorf("variant %s: %w", variant.Name, err)
			}
		}
		plan := snapshotActionPlan(variant.Snapshot)
		if err := substituteFamilyObservedHeaderInputs(plan, variant.Snapshot.ConfigDependencies, input.observedHeaderUses, input.observedHeaders); err != nil {
			return nil, fmt.Errorf("variant %s observed inputs: %w", variant.Name, err)
		}
		inputSetPruner := &familyPreciseInputSetPruner{plan: plan}
		prunedInputSets := false
		// Prune the evaluation-wide working set before structural matching. An
		// unrelated generated prerequisite can itself have config-specific recipe
		// content; retaining that edge until after structural IDs are assigned would
		// prevent otherwise identical compiler nodes from ever becoming candidates
		// for symmetric reduction.
		for nodeIndex := range plan.Nodes {
			node := &plan.Nodes[nodeIndex]
			originalInputSet := node.InputSet
			dependencies := variant.Snapshot.ConfigDependencies[node.ID]
			recipe, changed, err := prunePreciseFamilyCompilerInputsWithInputSets(plan, node, dependencies, inputSetPruner)
			if err != nil {
				return nil, fmt.Errorf("variant %s node %s precise input closure: %w", variant.Name, node.ID, err)
			}
			if !changed {
				continue
			}
			prunedInputSets = prunedInputSets || node.InputSet != originalInputSet
			recipeID, err := recipe.ID()
			if err != nil {
				return nil, fmt.Errorf("variant %s node %s precise input recipe: %w", variant.Name, node.ID, err)
			}
			plan.Recipes[recipeID] = cloneActionRecipe(recipe)
			node.Recipe = recipeID
		}
		if prunedInputSets {
			// Localization clones the public canonical store, not planner-only
			// history. Publish the new roots once after the complete pruning pass.
			if err := plan.exportReachableActionPlanInputSets(); err != nil {
				return nil, fmt.Errorf("variant %s precise input-set roots: %w", variant.Name, err)
			}
		}
		sourceIndex := make(map[string]ActionPlanSource, len(plan.Sources))
		for _, source := range plan.Sources {
			sourceIndex[source.ID] = source
		}
		hasConfigSource, err := familyConfigSourceUsage(plan, sourceIndex)
		if err != nil {
			return nil, fmt.Errorf("variant %s config-source index: %w", variant.Name, err)
		}
		structuralIDs, _, structuralInputSetRoots, err := familyNodeIDsWithOutputIdentity(
			plan, fullConfigSourceIdentity, structuralFamilyOutputPath, structuralFamilyArtifactPath,
		)
		if err != nil {
			return nil, fmt.Errorf("variant %s structural graph: %w", variant.Name, err)
		}
		reductions = append(reductions, familyVariantReduction{
			name: variant.Name, plan: plan,
			dependencies:            maps.Clone(variant.Snapshot.ConfigDependencies),
			configFiles:             maps.Clone(variant.Snapshot.ConfigFiles),
			structuralID:            structuralIDs,
			structuralInputSetRoots: structuralInputSetRoots,
			hasConfigSource:         hasConfigSource,
		})
	}
	unionSets, err := partitionFamilyStructuralCompatibility(reductions)
	if err != nil {
		return nil, err
	}

	family := &ActionPlanFamily{
		Toolsets: maps.Clone(reductions[0].plan.Toolsets), Recipes: map[string]ActionRecipe{},
		InputSets: map[string]ActionPlanInputSetNode{}, Capsules: map[string]map[string]string{}, Memberships: map[string][]string{},
		opaqueReasons: map[string]map[string]int{}, preciseCompileMemberships: map[string][]string{},
		originalNodeIDs: map[string]map[string]string{},
		executionCut:    executionCut,
	}
	// Opaque classifications are attached to final semantic nodes below. Keep
	// them node-addressed until reachability is known so discarded planner-only
	// work cannot survive in the reuse diagnostics.
	opaqueReasonNodes := map[string]map[string]map[string]bool{}
	for _, reduction := range reductions {
		family.opaqueReasons[reduction.name] = map[string]int{}
		opaqueReasonNodes[reduction.name] = map[string]map[string]bool{}
	}
	recipePayloads := map[string][]byte{}
	registerRecipe := func(variant, recipeID string, recipe ActionRecipe) error {
		if previous, ok := family.Recipes[recipeID]; ok {
			// Cross-config plans overwhelmingly repeat the same recipes. Avoid
			// serializing both values again unless equal content IDs somehow carry
			// distinct in-memory representations (for example nil versus empty
			// fields with the same canonical JSON).
			if reflect.DeepEqual(previous, recipe) {
				return nil
			}
			data, err := recipe.CanonicalJSON()
			if err != nil {
				return fmt.Errorf("variant %s recipe %s: %w", variant, recipeID, err)
			}
			previousData := recipePayloads[recipeID]
			if previousData == nil {
				previousData, _ = previous.CanonicalJSON()
				recipePayloads[recipeID] = previousData
			}
			if !bytes.Equal(previousData, data) {
				return fmt.Errorf("recipe content ID collision %s across image variants", recipeID)
			}
			return nil
		}
		data, err := recipe.CanonicalJSON()
		if err != nil {
			return fmt.Errorf("variant %s recipe %s: %w", variant, recipeID, err)
		}
		family.Recipes[recipeID] = cloneActionRecipe(recipe)
		recipePayloads[recipeID] = data
		return nil
	}
	type sourceDescriptor struct{ namespace, path string }
	sources := map[string]sourceDescriptor{}
	nodes := map[string]ActionPlanNode{}
	nodePayloads := map[string][]byte{}
	for _, reduction := range reductions {
		if !maps.Equal(family.Toolsets, reduction.plan.Toolsets) {
			return nil, fmt.Errorf("variant %s uses different host/target toolset identities", reduction.name)
		}
		family.Variants = append(family.Variants, reduction.name)
		for recipeID, recipe := range reduction.plan.Recipes {
			if err := registerRecipe(reduction.name, recipeID, recipe); err != nil {
				return nil, err
			}
		}

		oldSources := map[string]ActionPlanSource{}
		for _, source := range reduction.plan.Sources {
			oldSources[source.ID] = source
		}
		oldNodes := make(map[string]ActionPlanNode, len(reduction.plan.Nodes))
		for _, node := range reduction.plan.Nodes {
			oldNodes[node.ID] = node
		}
		registerCapsule := func(capsule ConfigCapsule) error {
			if previous, ok := family.Capsules[capsule.ID]; ok {
				if !maps.Equal(previous, capsule.Files) {
					return fmt.Errorf("config capsule content ID collision %s", capsule.ID)
				}
				return nil
			}
			family.Capsules[capsule.ID] = maps.Clone(capsule.Files)
			return nil
		}
		// Most nodes never consume resolved configuration, and thousands of
		// compiler nodes commonly share one dependency set. Render a capsule only
		// at an actual source/staging boundary and only once per byte-affecting
		// dependency set in this variant.
		nodeCapsules := map[string]ConfigCapsule{}
		capsuleCache := map[string]ConfigCapsule{}
		nodeCapsule := func(nodeID string) (ConfigCapsule, error) {
			if capsule, ok := nodeCapsules[nodeID]; ok {
				return capsule, nil
			}
			node, ok := oldNodes[nodeID]
			if !ok {
				return ConfigCapsule{}, fmt.Errorf("unknown family node %s", nodeID)
			}
			dependencies := unionSets[reduction.structuralID[node.ID]]
			dependencies = effectiveFamilyNodeConfigDependencies(reduction.plan, node, dependencies, reduction.hasConfigSource[node.ID])
			cacheKey := familyConfigCapsuleCacheKey(dependencies)
			if capsule, ok := capsuleCache[cacheKey]; ok {
				nodeCapsules[nodeID] = capsule
				return capsule, nil
			}
			capsule, err := RenderConfigCapsule(reduction.configFiles, dependencies)
			if err != nil {
				return ConfigCapsule{}, fmt.Errorf("variant %s node %s config capsule: %w", reduction.name, node.ID, err)
			}
			if err := registerCapsule(capsule); err != nil {
				return ConfigCapsule{}, err
			}
			capsuleCache[cacheKey] = capsule
			nodeCapsules[nodeID] = capsule
			return capsule, nil
		}

		localized := cloneActionPlan(reduction.plan)
		// Planner-owned private paths can contain a config-specific profile
		// digest. Structural matching above deliberately ignores those physical
		// allocations. Give every matched output one family-owned physical path
		// before final transitive IDs are computed, so both the producer identity
		// and every consumer ProjectionPath are stable across variants and input
		// order.
		for nodeIndex := range localized.Nodes {
			node := &localized.Nodes[nodeIndex]
			structuralID := reduction.structuralID[node.ID]
			if structuralID == "" {
				return nil, fmt.Errorf("variant %s node %s has no structural identity", reduction.name, node.ID)
			}
			for slot := range node.Outputs {
				if plannerOwnedObservedFamilyState(node.Outputs[slot]) {
					// Observed-state envelopes use Output.Path itself as a private
					// allocation. Relocate that path as one unit while retaining the
					// actual working-tree pathname in ObservedPath.
					node.Outputs[slot].Path = familyOwnedObservedStatePath(structuralID, slot)
					node.Outputs[slot].ArtifactPath = ""
				} else if _, private := plannerOwnedPrivateFamilyArtifact(node.Outputs[slot]); private {
					node.Outputs[slot].ArtifactPath = familyOwnedArtifactPath(structuralID, slot)
				}
			}
		}
		priorTreeRecipes, err := addFamilyPriorTreeInputs(
			reduction.plan, localized, reduction.structuralID, unionSets,
		)
		if err != nil {
			return nil, fmt.Errorf("variant %s prior-tree inputs: %w", reduction.name, err)
		}
		for recipeID, recipe := range priorTreeRecipes {
			if err := registerRecipe(reduction.name, recipeID, recipe); err != nil {
				return nil, err
			}
		}
		localizedNodes := make(map[string]ActionPlanNode, len(localized.Nodes))
		for _, node := range localized.Nodes {
			localizedNodes[node.ID] = node
		}
		localizedSources := map[string]ActionPlanSource{}
		registerSource := func(namespace, pathname string) (string, error) {
			id := semanticFamilySourceID(namespace, pathname)
			descriptor := sourceDescriptor{namespace: namespace, path: pathname}
			if previous, ok := sources[id]; ok && previous != descriptor {
				return "", fmt.Errorf("family source content ID collision %s", id)
			}
			sources[id] = descriptor
			localizedSources[id] = ActionPlanSource{ID: id, Namespace: namespace, Path: pathname}
			return id, nil
		}
		// A source can be consumed by several nodes with different dependency
		// sets, so replace direct config edges with node-local capsule sources.
		for nodeIndex := range localized.Nodes {
			node := &localized.Nodes[nodeIndex]
			var capsule ConfigCapsule
			capsuleReady := false
			for sourceIndex := range node.Sources {
				oldID := node.Sources[sourceIndex].SourceID
				source, ok := oldSources[oldID]
				if !ok {
					return nil, fmt.Errorf("variant %s node %s references unknown source %s", reduction.name, node.ID, oldID)
				}
				namespace, pathname := source.Namespace, source.Path
				if namespace == "config" {
					if !capsuleReady {
						capsule, err = nodeCapsule(node.ID)
						if err != nil {
							return nil, err
						}
						capsuleReady = true
					}
					namespace = "capsule"
					projection, ok := configSourceCapsulePath(source.Path)
					if !ok {
						return nil, fmt.Errorf("variant %s node %s references unknown config projection source %q", reduction.name, node.ID, source.Path)
					}
					pathname = path.Join(capsule.ID, projection)
				}
				id, err := registerSource(namespace, pathname)
				if err != nil {
					return nil, err
				}
				node.Sources[sourceIndex].SourceID = id
			}
		}
		// An opaque action which stages the prep tree has explicitly retained the
		// complete-directory contract: arbitrary source-owned code may inspect any
		// resolved configuration projection without naming it in argv. Make that
		// ambient state both an exact runtime input and part of the transitive family
		// identity. Precise compiler actions remain on their symbol capsules below.
		for nodeIndex := range localized.Nodes {
			node := &localized.Nodes[nodeIndex]
			dependencies := unionSets[reduction.structuralID[node.ID]]
			dependencies = effectiveFamilyNodeConfigDependencies(reduction.plan, oldNodes[node.ID], dependencies, reduction.hasConfigSource[node.ID])
			recipe, ok := localized.Recipes[node.Recipe]
			if !ok {
				return nil, fmt.Errorf("variant %s opaque node %s references unknown recipe %s", reduction.name, node.ID, node.Recipe)
			}
			if !dependencies.Opaque || !slices.Contains(recipe.WorkingTrees, "prep") {
				continue
			}
			// Recipes are interned and commonly shared by many nodes. Own every
			// mutable field before adding node-local ambient bindings.
			recipe = cloneActionRecipe(recipe)
			changed := false
			capsule, err := nodeCapsule(node.ID)
			if err != nil {
				return nil, err
			}
			staged := map[string]bool{}
			for _, pathname := range recipe.WorkingInputs {
				staged[canonicalKbuildRulePath(pathname)] = true
			}
			projected, err := familyNodePrivateTreeProjectionPaths(*node, recipe, localizedNodes, "prep")
			if err != nil {
				return nil, fmt.Errorf("variant %s opaque node %s prep-tree projections: %w", reduction.name, node.ID, err)
			}
			for pathname := range projected {
				staged[pathname] = true
			}
			for _, projection := range ResolvedConfigProjectionOutputs() {
				if staged[canonicalKbuildRulePath(projection)] {
					continue
				}
				sourceID, err := registerSource("capsule", path.Join(capsule.ID, projection))
				if err != nil {
					return nil, err
				}
				binding := "ambient-config:" + planOrdinal(len(node.Sources))
				node.Sources = append(node.Sources, ActionPlanSourceEdge{
					Role: "ambient-config", SourceID: sourceID,
				})
				recipe.Sources = append(recipe.Sources, binding)
				if recipe.WorkingInputs == nil {
					recipe.WorkingInputs = map[string]string{}
				}
				recipe.WorkingInputs["source:"+binding] = projection
				staged[projection] = true
				changed = true
			}
			if !changed {
				continue
			}
			recipeID, err := recipe.ID()
			if err != nil {
				return nil, fmt.Errorf("variant %s opaque node %s config-staging recipe: %w", reduction.name, node.ID, err)
			}
			if err := registerRecipe(reduction.name, recipeID, recipe); err != nil {
				return nil, err
			}
			localized.Recipes[recipeID] = cloneActionRecipe(recipe)
			node.Recipe = recipeID
		}

		sourceClosureRecipes, err := attachPreciseFamilySourceClosure(
			reduction.name, reduction.plan, localized, reduction.structuralID, unionSets, registerSource,
		)
		if err != nil {
			return nil, fmt.Errorf("variant %s exact source closure: %w", reduction.name, err)
		}
		for recipeID, recipe := range sourceClosureRecipes {
			if err := registerRecipe(reduction.name, recipeID, recipe); err != nil {
				return nil, err
			}
		}

		originalNodeIDs := make(map[string]bool, len(reduction.plan.Nodes))
		originalNodes := make(map[string]ActionPlanNode, len(reduction.plan.Nodes))
		localizedOriginals := make(map[string]ActionPlanNode, len(localized.Nodes))
		for index, node := range reduction.plan.Nodes {
			originalNodeIDs[node.ID] = true
			originalNodes[node.ID] = node
			localizedOriginals[node.ID] = localized.Nodes[index]
		}
		// Materialize precise config projections at the actual graph boundary.
		// The cloned copy has the same recipe/logical prep output, but its source
		// is the consuming compiler node's capsule. It is internal to the shared
		// store and therefore never becomes a variant public-tree view.
		localizedInputSets, err := localized.planningActionPlanInputSetStore()
		if err != nil {
			return nil, fmt.Errorf("variant %s localized input sets: %w", reduction.name, err)
		}
		fallbackProjections := map[string]string{}
		for id, producer := range originalNodes {
			if projection, fallback := canonicalFallbackConfigProjection(reduction.plan, oldSources, producer); fallback {
				fallbackProjections[id] = projection
			}
		}
		projectionClones := map[string]string{}
		projectionCloneCapsules := map[string]ConfigCapsule{}
		cloneProjection := func(producerID string, capsule ConfigCapsule) (string, error) {
			cloneKey := producerID + "\x00" + capsule.ID
			if cloneID := projectionClones[cloneKey]; cloneID != "" {
				return cloneID, nil
			}
			digest := sha256.Sum256([]byte("linux-kernel-family-config-projection-clone-v1\x00" + reduction.structuralID[producerID] + "\x00" + capsule.ID))
			cloneID := "projection-clone-" + hex.EncodeToString(digest[:])
			clone := localizedOriginals[producerID]
			clone.ID = cloneID
			clone.Sources = slices.Clone(clone.Sources)
			sourceID, err := registerSource("capsule", path.Join(capsule.ID, fallbackProjections[producerID]))
			if err != nil {
				return "", err
			}
			clone.Sources[0].SourceID = sourceID
			localized.Nodes = append(localized.Nodes, clone)
			projectionClones[cloneKey] = cloneID
			projectionCloneCapsules[cloneID] = capsule
			return cloneID, nil
		}
		// Only the few fallback-projection paths depend on the consumer capsule.
		// Summarize shared subtries once, then remap those paths without scanning
		// every cumulative working-tree root for every distinct capsule.
		fallbackSubtries := map[string]bool{}
		var containsFallbackProjection func(string) (bool, error)
		containsFallbackProjection = func(root string) (bool, error) {
			if root == "" {
				return false, nil
			}
			if contains, ok := fallbackSubtries[root]; ok {
				return contains, nil
			}
			node, err := localizedInputSets.nodeAt(root)
			if err != nil {
				return false, err
			}
			contains := false
			for _, entry := range node.Entries {
				if entry.Slot == 0 && fallbackProjections[entry.ProducerID] != "" {
					contains = true
					break
				}
			}
			if !contains {
				for _, child := range node.Children {
					childContains, err := containsFallbackProjection(child.ID)
					if err != nil {
						return false, err
					}
					if childContains {
						contains = true
						break
					}
				}
			}
			fallbackSubtries[root] = contains
			return contains, nil
		}
		projectionMappers := map[string]*ActionPlanInputSetTargetStableMapper{}
		originalCount := len(localized.Nodes)
		for nodeIndex := 0; nodeIndex < originalCount; nodeIndex++ {
			consumerID := localized.Nodes[nodeIndex].ID
			original := originalNodes[consumerID]
			dependencies := unionSets[reduction.structuralID[consumerID]]
			recipe, recipeOK := reduction.plan.Recipes[original.Recipe]
			projectedGenerator := recipeOK && len(recipe.ConfigProjectionPrefixes) != 0 && !dependencies.Opaque
			if !preciseFamilyCompilerNode(reduction.plan, original, dependencies) && !projectedGenerator {
				continue
			}
			var capsule ConfigCapsule
			capsuleReady := false
			for inputIndex := range localized.Nodes[nodeIndex].Inputs {
				input := localized.Nodes[nodeIndex].Inputs[inputIndex]
				if input.Slot != 0 || fallbackProjections[input.ProducerID] == "" {
					continue
				}
				if !capsuleReady {
					capsule, err = nodeCapsule(consumerID)
					if err != nil {
						return nil, err
					}
					capsuleReady = true
				}
				cloneID, err := cloneProjection(input.ProducerID, capsule)
				if err != nil {
					return nil, err
				}
				localized.Nodes[nodeIndex].Inputs[inputIndex].ProducerID = cloneID
			}
			root := localized.Nodes[nodeIndex].InputSet
			containsFallback, err := containsFallbackProjection(root)
			if err != nil {
				return nil, fmt.Errorf("variant %s node %s fallback input set: %w", reduction.name, consumerID, err)
			}
			if !containsFallback {
				continue
			}
			if !capsuleReady {
				capsule, err = nodeCapsule(consumerID)
				if err != nil {
					return nil, err
				}
			}
			mapper := projectionMappers[capsule.ID]
			if mapper == nil {
				mapper, err = localizedInputSets.newSelectiveTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
					if entry.Slot == 0 && fallbackProjections[entry.ProducerID] != "" {
						cloneID, err := cloneProjection(entry.ProducerID, capsule)
						if err != nil {
							return ActionPlanInputSetEntry{}, err
						}
						entry.ProducerID = cloneID
					}
					return entry, nil
				}, containsFallbackProjection)
				if err != nil {
					return nil, fmt.Errorf("variant %s capsule %s projection mapper: %w", reduction.name, capsule.ID, err)
				}
				projectionMappers[capsule.ID] = mapper
			}
			root, err = mapper.Map(root)
			if err != nil {
				return nil, fmt.Errorf("variant %s node %s projected input set: %w", reduction.name, consumerID, err)
			}
			localized.Nodes[nodeIndex].InputSet = root
		}
		sharedSourceMapper, err := localizedInputSets.NewTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
			if entry.SourceID == "" {
				return entry, nil
			}
			if _, ok := localizedSources[entry.SourceID]; ok {
				return entry, nil
			}
			source, ok := oldSources[entry.SourceID]
			if !ok {
				return ActionPlanInputSetEntry{}, fmt.Errorf("variant %s input set references unknown source %s", reduction.name, entry.SourceID)
			}
			if source.Namespace == "config" {
				return entry, nil
			}
			id, err := registerSource(source.Namespace, source.Path)
			if err != nil {
				return ActionPlanInputSetEntry{}, err
			}
			entry.SourceID = id
			return entry, nil
		})
		if err != nil {
			return nil, fmt.Errorf("variant %s shared-source input-set mapper: %w", reduction.name, err)
		}
		containsConfigMemo := map[string]bool{}
		containsConfigKnown := map[string]bool{}
		var inputSetContainsConfigSource func(string) (bool, error)
		inputSetContainsConfigSource = func(root string) (bool, error) {
			if root == "" {
				return false, nil
			}
			if containsConfigKnown[root] {
				return containsConfigMemo[root], nil
			}
			inputSetNode, ok := localizedInputSets.Node(root)
			if !ok {
				return false, fmt.Errorf("input-set node %s is missing", root)
			}
			contains := false
			for _, entry := range inputSetNode.Entries {
				if source, ok := oldSources[entry.SourceID]; ok && source.Namespace == "config" {
					contains = true
					break
				}
			}
			if !contains {
				for _, child := range inputSetNode.Children {
					childContains, err := inputSetContainsConfigSource(child.ID)
					if err != nil {
						return false, err
					}
					if childContains {
						contains = true
						break
					}
				}
			}
			containsConfigKnown[root] = true
			containsConfigMemo[root] = contains
			return contains, nil
		}
		capsuleMappers := map[string]*ActionPlanInputSetTargetStableMapper{}
		for nodeIndex := range localized.Nodes {
			node := &localized.Nodes[nodeIndex]
			root, err := sharedSourceMapper.Map(node.InputSet)
			if err != nil {
				return nil, fmt.Errorf("variant %s node %s shared-source input set: %w", reduction.name, node.ID, err)
			}
			containsConfig, err := inputSetContainsConfigSource(root)
			if err != nil {
				return nil, fmt.Errorf("variant %s node %s config-source input set: %w", reduction.name, node.ID, err)
			}
			if containsConfig {
				capsule, ok := projectionCloneCapsules[node.ID]
				if !ok {
					capsule, err = nodeCapsule(node.ID)
					if err != nil {
						return nil, err
					}
				}
				mapper := capsuleMappers[capsule.ID]
				if mapper == nil {
					mapper, err = localizedInputSets.newSelectiveTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
						source, ok := oldSources[entry.SourceID]
						if !ok || source.Namespace != "config" {
							return entry, nil
						}
						projection, ok := configSourceCapsulePath(source.Path)
						if !ok {
							return ActionPlanInputSetEntry{}, fmt.Errorf("variant %s input set references unknown config projection source %q", reduction.name, source.Path)
						}
						id, err := registerSource("capsule", path.Join(capsule.ID, projection))
						if err != nil {
							return ActionPlanInputSetEntry{}, err
						}
						entry.SourceID = id
						return entry, nil
					}, inputSetContainsConfigSource)
					if err != nil {
						return nil, fmt.Errorf("variant %s capsule %s input-set mapper: %w", reduction.name, capsule.ID, err)
					}
					capsuleMappers[capsule.ID] = mapper
				}
				root, err = mapper.Map(root)
			}
			if err != nil {
				return nil, fmt.Errorf("variant %s node %s localized input set: %w", reduction.name, node.ID, err)
			}
			node.InputSet = root
		}
		localized.Sources = localized.Sources[:0]
		for _, source := range localizedSources {
			localized.Sources = append(localized.Sources, source)
		}
		sort.Slice(localized.Sources, func(i, j int) bool { return localized.Sources[i].ID < localized.Sources[j].ID })
		if err := relocateFinalPreciseFamilyOutputs(reduction.plan, localized, reduction.structuralID, unionSets); err != nil {
			return nil, fmt.Errorf("variant %s final precise output allocation: %w", reduction.name, err)
		}
		finalIDs, payloads, finalInputSetRoots, err := familyNodeIDs(localized, func(source ActionPlanSource) string { return source.ID })
		if err != nil {
			return nil, fmt.Errorf("variant %s final graph: %w", reduction.name, err)
		}
		roots := make([]string, 0, len(finalInputSetRoots))
		for _, root := range finalInputSetRoots {
			roots = append(roots, root)
		}
		closure, err := localizedInputSets.ReachableNodesForRoots(roots)
		if err != nil {
			return nil, fmt.Errorf("variant %s final input sets: %w", reduction.name, err)
		}
		if err := mergeActionPlanFamilyInputSetNodes(family.InputSets, closure); err != nil {
			return nil, fmt.Errorf("variant %s final input sets: %w", reduction.name, err)
		}
		reduction.finalID = finalIDs
		family.originalNodeIDs[reduction.name] = make(map[string]string, len(reduction.plan.Nodes))
		for _, original := range reduction.plan.Nodes {
			family.originalNodeIDs[reduction.name][original.ID] = finalIDs[original.ID]
		}
		exactViews := map[ActionPlanFamilyView]bool{}
		for _, localizedNode := range localized.Nodes {
			provisionalID := localizedNode.ID
			id := finalIDs[provisionalID]
			node := localizedNode
			node.ID = id
			node.InputSet = finalInputSetRoots[provisionalID]
			node.Inputs = slices.Clone(node.Inputs)
			for index := range node.Inputs {
				node.Inputs[index].ProducerID = finalIDs[node.Inputs[index].ProducerID]
			}
			node.Trees = slices.Clone(node.Trees)
			node.AuxiliaryTools = slices.Clone(node.AuxiliaryTools)
			node.Outputs = slices.Clone(node.Outputs)
			node.familySourceProjections = slices.Clone(node.familySourceProjections)
			if previous, ok := nodes[id]; ok {
				if !bytes.Equal(nodePayloads[id], payloads[provisionalID]) {
					return nil, fmt.Errorf("semantic node content ID collision %s", id)
				}
				// Product is facade metadata. Pick one deterministic marker value.
				if node.Product < previous.Product {
					previous.Product = node.Product
					nodes[id] = previous
				}
			} else {
				nodes[id] = node
				nodePayloads[id] = slices.Clone(payloads[provisionalID])
			}
			family.Memberships[id] = append(family.Memberships[id], reduction.name)
			if !originalNodeIDs[provisionalID] {
				continue
			}
			original := originalNodes[provisionalID]
			dependencies := unionSets[reduction.structuralID[provisionalID]]
			dependencies = effectiveFamilyNodeConfigDependencies(reduction.plan, original, dependencies, reduction.hasConfigSource[original.ID])
			if dependencies.Opaque {
				// Report the effective symmetric classification for every member:
				// a locally precise node paired with an opaque peer must still carry
				// the reason which forced its full-config capsule. The per-node set
				// avoids double counting planner-private copies which reduce to the
				// same final semantic node within one variant.
				reasons := opaqueReasonNodes[reduction.name][id]
				if reasons == nil {
					reasons = map[string]bool{}
					opaqueReasonNodes[reduction.name][id] = reasons
				}
				reasons[node.Kind+"\x00"+dependencies.Reason] = true
			}
			if preciseFamilyCompilerNode(reduction.plan, original, dependencies) {
				family.preciseCompileMemberships[id] = append(family.preciseCompileMemberships[id], reduction.name)
			}
			for slot, output := range localizedNode.Outputs {
				if projectedGeneratorOutputIsInternal(reduction.plan, provisionalID, slot) {
					continue
				}
				if !familyViewTree(output.Tree) {
					continue
				}
				view := ActionPlanFamilyView{
					Variant: reduction.name, Tree: output.Tree, NodeID: id, Slot: slot,
					ArtifactPath: actionPlanOutputArtifactPath(output),
				}
				// Planner-private physical versions can reduce to the exact same
				// semantic producer within one variant. Publish that reduced output
				// once, while retaining distinct tuples so validation still rejects
				// genuinely conflicting owners of one variant/tree/path.
				if !exactViews[view] {
					exactViews[view] = true
					reduction.views = append(reduction.views, view)
				}
			}
		}
		for _, validation := range reduction.plan.projectedGeneratorValidations {
			id := finalIDs[validation]
			if id == "" {
				return nil, fmt.Errorf("variant %s validation root %s has no final identity", reduction.name, validation)
			}
			reduction.validations = append(reduction.validations, ActionPlanFamilyValidation{
				Variant: reduction.name, NodeID: id, Slot: 0,
			})
		}
		for _, product := range reduction.plan.Products {
			reduction.products = append(reduction.products, ActionPlanFamilyProduct{
				Variant: reduction.name, Name: product.Name, Tree: product.Tree, Path: product.Path,
			})
		}
		family.Views = append(family.Views, reduction.views...)
		family.Products = append(family.Products, reduction.products...)
		family.Validations = append(family.Validations, reduction.validations...)
	}
	familyInputSets, err := NewActionPlanInputSetStoreFromNodes(family.InputSets)
	if err != nil {
		return nil, fmt.Errorf("family input sets: %w", err)
	}
	executionRoots, err := family.executedCutRoots()
	if err != nil {
		return nil, err
	}
	reachableMemberships, err := reachableActionPlanFamilyMemberships(
		nodes, familyInputSets, family.Memberships, family.Variants, family.Views, family.Products, family.Validations, executionRoots,
	)
	if err != nil {
		return nil, err
	}
	family.Memberships = reachableMemberships
	// Snapshot nodes which became unreachable in one variant are not part of
	// that variant's execution contract, even if another variant retains the
	// same semantic node. Keep provenance aligned with the final live graph.
	for variant, originals := range family.originalNodeIDs {
		for original, id := range originals {
			if !slices.Contains(reachableMemberships[id], variant) {
				delete(originals, original)
			}
		}
	}

	// Keep only execution metadata owned by the live graph. Source edges retain
	// capsule projections; exact input edges and whole-tree closure above retain
	// their producers; auxiliary tools remain embedded in each retained node.
	retainedRecipes := map[string]ActionRecipe{}
	retainedSourceIDs := map[string]bool{}
	retainedInputSetRoots := []string{}
	for id, node := range nodes {
		if len(reachableMemberships[id]) == 0 {
			continue
		}
		family.Nodes = append(family.Nodes, node)
		recipe, ok := family.Recipes[node.Recipe]
		if !ok {
			return nil, fmt.Errorf("reachable family node %s references unknown recipe %s", id, node.Recipe)
		}
		retainedRecipes[node.Recipe] = recipe
		for _, source := range node.Sources {
			retainedSourceIDs[source.SourceID] = true
		}
		retainedInputSetRoots = append(retainedInputSetRoots, node.InputSet)
	}
	retainedInputSets, err := familyInputSets.ReachableNodesForRoots(retainedInputSetRoots)
	if err != nil {
		return nil, fmt.Errorf("reachable family input sets: %w", err)
	}
	for _, inputSetNode := range retainedInputSets {
		for _, entry := range inputSetNode.Entries {
			if entry.SourceID != "" {
				retainedSourceIDs[entry.SourceID] = true
			}
		}
	}
	family.Recipes = retainedRecipes
	family.InputSets = retainedInputSets
	retainedCapsules := map[string]map[string]string{}
	for id := range retainedSourceIDs {
		source, ok := sources[id]
		if !ok {
			return nil, fmt.Errorf("reachable family graph references unknown source %s", id)
		}
		family.Sources = append(family.Sources, ActionPlanSource{ID: id, Namespace: source.namespace, Path: source.path})
		if source.namespace != "capsule" {
			continue
		}
		digest, _, ok := strings.Cut(source.path, "/")
		if !ok || family.Capsules[digest] == nil {
			return nil, fmt.Errorf("reachable family source %s refers to unknown config capsule %q", id, source.path)
		}
		retainedCapsules[digest] = family.Capsules[digest]
	}
	family.Capsules = retainedCapsules

	for variant, reasonsByNode := range opaqueReasonNodes {
		for id, reasons := range reasonsByNode {
			if !slices.Contains(reachableMemberships[id], variant) {
				continue
			}
			for reason := range reasons {
				family.opaqueReasons[variant][reason]++
			}
		}
	}
	for id, memberships := range family.Memberships {
		sort.Strings(memberships)
		family.Memberships[id] = slices.Compact(memberships)
	}
	for id, memberships := range family.preciseCompileMemberships {
		memberships = slices.DeleteFunc(memberships, func(variant string) bool {
			return !slices.Contains(reachableMemberships[id], variant)
		})
		if len(memberships) == 0 {
			delete(family.preciseCompileMemberships, id)
			continue
		}
		sort.Strings(memberships)
		family.preciseCompileMemberships[id] = slices.Compact(memberships)
	}
	sort.Slice(family.Sources, func(i, j int) bool { return family.Sources[i].ID < family.Sources[j].ID })
	sort.Slice(family.Nodes, func(i, j int) bool { return family.Nodes[i].ID < family.Nodes[j].ID })
	sort.Slice(family.Views, func(i, j int) bool {
		left, right := family.Views[i], family.Views[j]
		return strings.Join([]string{left.Variant, left.Tree, left.ArtifactPath, left.NodeID, planOrdinal(left.Slot)}, "\x00") <
			strings.Join([]string{right.Variant, right.Tree, right.ArtifactPath, right.NodeID, planOrdinal(right.Slot)}, "\x00")
	})
	sort.Slice(family.Products, func(i, j int) bool {
		left, right := family.Products[i], family.Products[j]
		return strings.Join([]string{left.Variant, left.Name, left.Tree, left.Path}, "\x00") <
			strings.Join([]string{right.Variant, right.Name, right.Tree, right.Path}, "\x00")
	})
	sort.Slice(family.Validations, func(i, j int) bool {
		left, right := family.Validations[i], family.Validations[j]
		return strings.Join([]string{left.Variant, left.NodeID, planOrdinal(left.Slot)}, "\x00") <
			strings.Join([]string{right.Variant, right.NodeID, planOrdinal(right.Slot)}, "\x00")
	})
	if err := family.validateBuiltWithStats(validationStats); err != nil {
		return nil, err
	}
	return &validatedActionPlanFamily{family: family}, nil
}

// BuildActionPlanFamily performs a symmetric reduction and returns the
// validated family for inspection or standalone emission.
func BuildActionPlanFamily(variants []ActionPlanFamilyVariant) (*ActionPlanFamily, error) {
	inputs := make([]actionPlanFamilyBuildVariant, 0, len(variants))
	for _, variant := range variants {
		inputs = append(inputs, actionPlanFamilyBuildVariant{variant: variant})
	}
	validated, err := buildValidatedActionPlanFamily(inputs)
	if err != nil {
		return nil, err
	}
	return validated.family, nil
}

// relocateFinalPreciseFamilyOutputs removes allocation salts left behind by
// structural compatibility partitioning. A fallback config producer can be
// config-specific during partitioning and then become one precise projection
// during localization. Its compiler's private output paths must follow that
// final input boundary too. Opaque nodes (including retained executions) keep
// their original allocations; final node IDs still hash every actual path.
func relocateFinalPreciseFamilyOutputs(original, localized *ActionPlan, structuralIDs map[string]string, unionSets map[string]ConfigDependencySet) error {
	// This is only an identity view: share immutable catalogs and the persistent
	// input-set store, and copy outputs solely for eligible original nodes. It
	// neither mutates recipes nor treats a family-looking path as ownership.
	identity := *localized
	identity.Nodes = slices.Clone(localized.Nodes)
	ordinals := make(map[string]int, len(identity.Nodes))
	for index, node := range identity.Nodes {
		ordinals[node.ID] = index
	}
	owned := map[int]map[int]bool{} // node ordinal -> output slot -> observed state
	for _, node := range original.Nodes {
		structuralID := structuralIDs[node.ID]
		dependencies, classified := unionSets[structuralID]
		if structuralID == "" || !classified {
			return fmt.Errorf("precise output allocation requires an original dependency classification")
		}
		if !preciseFamilyCompilerNode(original, node, dependencies) {
			continue
		}
		index, found := ordinals[node.ID]
		if !found || len(identity.Nodes[index].Outputs) != len(node.Outputs) {
			return fmt.Errorf("precise output allocation lost its original node or slots")
		}
		for slot, output := range node.Outputs {
			observed := plannerOwnedObservedFamilyState(output)
			_, private := plannerOwnedPrivateFamilyArtifact(output)
			if !observed && !private {
				continue
			}
			if owned[index] == nil {
				owned[index] = map[int]bool{}
				identity.Nodes[index].Outputs = slices.Clone(identity.Nodes[index].Outputs)
			}
			owned[index][slot] = observed
			identity.Nodes[index].Outputs[slot].Path = structuralFamilyOutputPath(output)
			identity.Nodes[index].Outputs[slot].ArtifactPath = structuralFamilyArtifactPath(output)
		}
	}
	if len(owned) == 0 {
		return nil
	}
	allocationIDs, _, _, err := familyNodeIDs(&identity, func(source ActionPlanSource) string { return source.ID })
	if err != nil {
		return err
	}
	for index, slots := range owned {
		node := &localized.Nodes[index]
		for slot, observed := range slots {
			if observed {
				node.Outputs[slot].Path = familyOwnedObservedStatePath(allocationIDs[node.ID], slot)
				node.Outputs[slot].ArtifactPath = ""
			} else {
				node.Outputs[slot].ArtifactPath = familyOwnedArtifactPath(allocationIDs[node.ID], slot)
			}
		}
	}
	return nil
}

func familyViewTree(tree string) bool {
	switch tree {
	case "objects", "sdk", "vmlinux", "image", "modules", "metadata":
		return true
	default:
		return false
	}
}

func familyStageOrdinal(stage string) int {
	for ordinal, candidate := range linuxKernelPlanStageOrder {
		if candidate == stage {
			return ordinal
		}
	}
	return len(linuxKernelPlanStageOrder)
}

// reachableActionPlanFamilyMemberships computes liveness after every variant
// has received its final content-addressed node identities. Doing this earlier
// would make a variant's dead planner work influence symmetric matching; doing
// it here also lets one shared semantic node retain only the variants which can
// actually reach it.
//
// Node.Inputs and persistent input-set producer leaves together form the
// complete generated-file dependency relation. Family reduction lowers
// strict-prior-stage WorkingTrees visibility into exact inputs before final
// identities are computed, so liveness and execution close over the same
// edges.
func reachableActionPlanFamilyMemberships(
	nodes map[string]ActionPlanNode,
	inputSets *ActionPlanInputSetStore,
	memberships map[string][]string,
	variants []string,
	views []ActionPlanFamilyView,
	products []ActionPlanFamilyProduct,
	validations []ActionPlanFamilyValidation,
	executionRoots map[string][]string,
) (map[string][]string, error) {
	type treeProducerIndex map[string]map[string]bool
	outputProducers := make(map[string]treeProducerIndex, len(variants))
	for _, variant := range variants {
		outputProducers[variant] = treeProducerIndex{}
	}
	for nodeID, node := range nodes {
		for _, variant := range memberships[nodeID] {
			index := outputProducers[variant]
			if index == nil {
				return nil, fmt.Errorf("family node %s has unknown variant membership %q", nodeID, variant)
			}
			for _, output := range node.Outputs {
				producers := index[output.Tree]
				if producers == nil {
					producers = map[string]bool{}
					index[output.Tree] = producers
				}
				producers[nodeID] = true
			}
		}
	}

	roots := make(map[string]map[string]bool, len(variants))
	for _, variant := range variants {
		roots[variant] = map[string]bool{}
	}
	for variant, nodeIDs := range executionRoots {
		if roots[variant] == nil {
			return nil, fmt.Errorf("family execution retention has unknown variant %q", variant)
		}
		for _, nodeID := range nodeIDs {
			roots[variant][nodeID] = true
		}
	}
	for _, view := range views {
		if roots[view.Variant] == nil {
			return nil, fmt.Errorf("family view has unknown variant %q", view.Variant)
		}
		roots[view.Variant][view.NodeID] = true
	}
	for _, validation := range validations {
		if roots[validation.Variant] == nil {
			return nil, fmt.Errorf("family validation has unknown variant %q", validation.Variant)
		}
		roots[validation.Variant][validation.NodeID] = true
	}
	for _, product := range products {
		variantRoots := roots[product.Variant]
		if variantRoots == nil {
			return nil, fmt.Errorf("family product has unknown variant %q", product.Variant)
		}
		// Views are exact, membership-independent roots for every public output
		// tree. Do not derive roots for those trees from the membership map that
		// this function is meant to reconstruct: doing so would let a forged
		// membership make itself reachable through a tree-root product.
		if familyViewTree(product.Tree) {
			if product.Path == LinuxKernelTreeRootMarker {
				continue
			}
			found := false
			for _, view := range views {
				if view.Variant != product.Variant || view.Tree != product.Tree {
					continue
				}
				node, ok := nodes[view.NodeID]
				if !ok || view.Slot < 0 || view.Slot >= len(node.Outputs) {
					continue
				}
				output := node.Outputs[view.Slot]
				if actionPlanOutputIsCanonical(output) && output.Path == product.Path {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("family product %s/%s refers to unowned output %s/%s", product.Variant, product.Name, product.Tree, product.Path)
			}
			continue
		}
		if product.Path == LinuxKernelTreeRootMarker {
			for nodeID := range outputProducers[product.Variant][product.Tree] {
				variantRoots[nodeID] = true
			}
			continue
		}
		found := false
		for nodeID := range outputProducers[product.Variant][product.Tree] {
			for _, output := range nodes[nodeID].Outputs {
				if output.Tree == product.Tree && actionPlanOutputIsCanonical(output) && output.Path == product.Path {
					variantRoots[nodeID] = true
					found = true
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("family product %s/%s refers to unowned output %s/%s", product.Variant, product.Name, product.Tree, product.Path)
		}
	}

	reachable := map[string][]string{}
	for _, variant := range variants {
		seen := map[string]bool{}
		seenInputSets := map[string]bool{}
		var visit func(string) error
		var visitInputSet func(string) error
		visitInputSet = func(inputSetID string) error {
			if inputSetID == "" || seenInputSets[inputSetID] {
				return nil
			}
			inputSetNode, ok := inputSets.Node(inputSetID)
			if !ok {
				return fmt.Errorf("family reachability references unknown input-set node %s", inputSetID)
			}
			seenInputSets[inputSetID] = true
			for _, entry := range inputSetNode.Entries {
				if entry.ProducerID != "" {
					if err := visit(entry.ProducerID); err != nil {
						return err
					}
				}
			}
			for _, child := range inputSetNode.Children {
				if err := visitInputSet(child.ID); err != nil {
					return err
				}
			}
			return nil
		}
		visit = func(nodeID string) error {
			if seen[nodeID] {
				return nil
			}
			node, ok := nodes[nodeID]
			if !ok {
				return fmt.Errorf("family reachability references unknown node %s", nodeID)
			}
			if !slices.Contains(memberships[nodeID], variant) {
				return fmt.Errorf("family variant %s reaches node %s without membership", variant, nodeID)
			}
			seen[nodeID] = true
			reachable[nodeID] = append(reachable[nodeID], variant)
			for _, input := range node.Inputs {
				if err := visit(input.ProducerID); err != nil {
					return err
				}
			}
			if err := visitInputSet(node.InputSet); err != nil {
				return fmt.Errorf("family node %s input set: %w", nodeID, err)
			}
			return nil
		}
		for nodeID := range roots[variant] {
			if err := visit(nodeID); err != nil {
				return nil, err
			}
		}
	}
	return reachable, nil
}

func familyStorePath(nodeID string, slot int) string {
	return path.Join("nodes", nodeID, planOrdinal(slot))
}

func validFamilySourceID(value string) bool {
	digest, ok := strings.CutPrefix(value, "src-")
	return ok && validatePlanDigest("family source ID", digest) == nil
}

func validateFamilyMembershipList(subject string, memberships, variants []string) error {
	if len(memberships) == 0 {
		return fmt.Errorf("%s has no variant membership", subject)
	}
	for index, variant := range memberships {
		if !seenVariantsSlice(variants, variant) {
			return fmt.Errorf("%s has unknown variant membership %q", subject, variant)
		}
		if index == 0 {
			continue
		}
		if memberships[index-1] == variant {
			return fmt.Errorf("%s repeats variant membership %q", subject, variant)
		}
		if memberships[index-1] > variant {
			return fmt.Errorf("%s variant memberships are not in canonical order", subject)
		}
	}
	return nil
}

// actionPlanFamilyValidationStats is private test instrumentation for the
// source-projection validation path. A closure with P source projections must
// build each retained recipe binding index once and perform exactly P indexed
// lookups; it must never scan a closure-sized recipe binding slice P times.
type actionPlanFamilyValidationStats struct {
	recipeSourceBindingsIndexed    int
	sourceProjectionBindingLookups int
	semanticNodesRehashed          int
	semanticSourceEdgesRehashed    int
	semanticInputEdgesRehashed     int
}

func (f *ActionPlanFamily) validate() error {
	return f.validateWithStats(nil)
}

func (f *ActionPlanFamily) validateWithStats(stats *actionPlanFamilyValidationStats) error {
	return f.validateRepresentation(stats, false)
}

// validateBuiltWithStats is valid only while buildValidatedActionPlanFamily
// still owns the family. Every semantic node ID was computed from the localized
// plan immediately before merging. familyNodeIDs also rejects cycles, and
// equal semantic IDs have equal dependency edges, so merging those validated
// DAGs cannot introduce a cycle. The builder changes only Product (which is
// deliberately excluded from semanticFamilyNodeBytes) afterward. Keep every
// remaining structural, reference, membership, and reachability check while
// avoiding repeated traversal and hashing of dense transitive input sets.
func (f *ActionPlanFamily) validateBuiltWithStats(stats *actionPlanFamilyValidationStats) error {
	return f.validateRepresentation(stats, true)
}

func (f *ActionPlanFamily) validateRepresentation(stats *actionPlanFamilyValidationStats, semanticIDsAlreadyValidated bool) error {
	if f == nil {
		return fmt.Errorf("kernel action plan family is nil")
	}
	if len(f.Variants) == 0 {
		return fmt.Errorf("kernel action plan family has no variants")
	}
	if !sort.StringsAreSorted(f.Variants) {
		return fmt.Errorf("kernel action plan family variants are not in canonical order")
	}
	for index, variant := range f.Variants {
		if err := validatePlanName("family variant", variant); err != nil {
			return err
		}
		if index != 0 && variant == f.Variants[index-1] {
			return fmt.Errorf("kernel action plan family repeats variant %q", variant)
		}
	}
	for scope, identity := range f.Toolsets {
		if scope != "host" && scope != "target" {
			return fmt.Errorf("kernel action plan family has unknown toolset scope %q", scope)
		}
		if err := validateProbeIdentity(identity); err != nil {
			return fmt.Errorf("family %s toolset: %w", scope, err)
		}
	}
	if f.Toolsets["target"] == "" {
		return fmt.Errorf("kernel action plan family has no target toolset")
	}

	recipeSourceBindings := make(map[string]map[string]struct{}, len(f.Recipes))
	for id, recipe := range f.Recipes {
		if err := validatePlanDigest("family recipe ID", id); err != nil {
			return err
		}
		actual, err := recipe.ID()
		if err != nil {
			return fmt.Errorf("family recipe %s: %w", id, err)
		}
		if actual != id {
			return fmt.Errorf("family recipe ID %s does not match canonical content %s", id, actual)
		}
	}

	sources := make(map[string]ActionPlanSource, len(f.Sources))
	for _, source := range f.Sources {
		if !validFamilySourceID(source.ID) {
			return fmt.Errorf("invalid family source ID %q", source.ID)
		}
		if err := validatePlanName("family source namespace", source.Namespace); err != nil {
			return err
		}
		if err := validatePlanRelativePath("family source", source.Path); err != nil {
			return err
		}
		if want := semanticFamilySourceID(source.Namespace, source.Path); source.ID != want {
			return fmt.Errorf("family source ID %s does not match semantic content %s", source.ID, want)
		}
		if _, exists := sources[source.ID]; exists {
			return fmt.Errorf("kernel action plan family repeats source %s", source.ID)
		}
		sources[source.ID] = source
		if source.Namespace == "capsule" {
			digest, relative, ok := strings.Cut(source.Path, "/")
			if !ok || validatePlanDigest("config capsule ID", digest) != nil || f.Capsules[digest] == nil {
				return fmt.Errorf("family source %s refers to unknown config capsule %q", source.ID, source.Path)
			}
			if _, ok := f.Capsules[digest][relative]; !ok {
				return fmt.Errorf("family source %s refers to missing config capsule file %q", source.ID, source.Path)
			}
		}
	}
	for digest, files := range f.Capsules {
		if err := validatePlanDigest("config capsule ID", digest); err != nil {
			return err
		}
		if len(files) != len(ResolvedConfigProjectionOutputs()) {
			return fmt.Errorf("config capsule %s has %d projections, want %d", digest, len(files), len(ResolvedConfigProjectionOutputs()))
		}
		for _, pathname := range ResolvedConfigProjectionOutputs() {
			if _, ok := files[pathname]; !ok {
				return fmt.Errorf("config capsule %s is missing %q", digest, pathname)
			}
		}
		if actual := configCapsuleID(files); actual != digest {
			return fmt.Errorf("config capsule ID %s does not match canonical content %s", digest, actual)
		}
	}

	nodes := make(map[string]ActionPlanNode, len(f.Nodes))
	for _, node := range f.Nodes {
		if err := validatePlanDigest("family node ID", node.ID); err != nil {
			return err
		}
		if _, exists := nodes[node.ID]; exists {
			return fmt.Errorf("kernel action plan family repeats node %s", node.ID)
		}
		if !LinuxKernelPlanStages[node.Stage] || !LinuxKernelPlanNodeKinds[node.Kind] {
			return fmt.Errorf("family node %s has unsupported stage/kind %q/%q", node.ID, node.Stage, node.Kind)
		}
		if !LinuxKernelPlanProducts[node.Product] {
			return fmt.Errorf("family node %s has unknown product %q", node.ID, node.Product)
		}
		recipe, ok := f.Recipes[node.Recipe]
		if !ok {
			return fmt.Errorf("family node %s references unknown recipe %s", node.ID, node.Recipe)
		}
		if recipe.Kind != node.Kind {
			return fmt.Errorf("family node %s kind differs from its recipe", node.ID)
		}
		if strings.HasPrefix(recipe.Tool, "input:") {
			if node.Tool != "generated" {
				return fmt.Errorf("family node %s generated recipe has marker tool %q", node.ID, node.Tool)
			}
		} else if recipe.Tool != node.Tool {
			return fmt.Errorf("family node %s tool differs from its recipe", node.ID)
		}
		if len(node.Outputs) == 0 {
			return fmt.Errorf("family node %s has no outputs", node.ID)
		}
		for _, edge := range node.Sources {
			if _, ok := sources[edge.SourceID]; !ok {
				return fmt.Errorf("family node %s references unknown source %s", node.ID, edge.SourceID)
			}
		}
		if len(node.familySourceProjections) > maximumActionPlanSourceProjectionCount {
			return fmt.Errorf(
				"family node %s has %d source projections, want at most %d",
				node.ID, len(node.familySourceProjections), maximumActionPlanSourceProjectionCount,
			)
		}
		seenProjectionSources := map[int]bool{}
		seenProjectionDestinations := map[string]bool{}
		sourceBindings := recipeSourceBindings[node.Recipe]
		if len(node.familySourceProjections) != 0 && sourceBindings == nil {
			sourceBindings = make(map[string]struct{}, len(recipe.Sources))
			for _, binding := range recipe.Sources {
				sourceBindings[binding] = struct{}{}
			}
			recipeSourceBindings[node.Recipe] = sourceBindings
			if stats != nil {
				stats.recipeSourceBindingsIndexed += len(recipe.Sources)
			}
		}
		for index, projection := range node.familySourceProjections {
			if index != 0 && !familySourceProjectionLess(node.familySourceProjections[index-1], projection) {
				return fmt.Errorf("family node %s source projections are not in canonical order", node.ID)
			}
			if projection.SourceOrdinal < 0 || projection.SourceOrdinal >= len(node.Sources) {
				return fmt.Errorf("family node %s source projection has invalid source ordinal %d", node.ID, projection.SourceOrdinal)
			}
			if seenProjectionSources[projection.SourceOrdinal] {
				return fmt.Errorf("family node %s repeats source projection ordinal %d", node.ID, projection.SourceOrdinal)
			}
			seenProjectionSources[projection.SourceOrdinal] = true
			if projection.Tree != "kernel" {
				return fmt.Errorf("family node %s source projection has non-kernel tree %q", node.ID, projection.Tree)
			}
			if err := validatePlanRelativePath("family source projection", projection.Path); err != nil {
				return err
			}
			destination := projection.Tree + "\x00" + projection.Path
			if seenProjectionDestinations[destination] {
				return fmt.Errorf("family node %s repeats source projection destination %s/%s", node.ID, projection.Tree, projection.Path)
			}
			seenProjectionDestinations[destination] = true
			edge := node.Sources[projection.SourceOrdinal]
			source := sources[edge.SourceID]
			if source.Namespace == "config" || source.Namespace == "capsule" || source.Path != projection.Path {
				return fmt.Errorf(
					"family node %s source projection %s/%s disagrees with source %s/%s",
					node.ID, projection.Tree, projection.Path, source.Namespace, source.Path,
				)
			}
			binding := edge.Role + ":" + planOrdinal(projection.SourceOrdinal)
			if stats != nil {
				stats.sourceProjectionBindingLookups++
			}
			if _, ok := sourceBindings[binding]; !ok {
				return fmt.Errorf("family node %s source projection binding %q is absent from its recipe", node.ID, binding)
			}
			if !slices.Contains(node.Trees, projection.Tree) || !slices.Contains(recipe.Trees, projection.Tree) {
				return fmt.Errorf("family node %s source projection tree %q is not an exact node/recipe tree binding", node.ID, projection.Tree)
			}
		}
		for slot, output := range node.Outputs {
			if !LinuxKernelPlanTrees[output.Tree] {
				return fmt.Errorf("family node %s has unknown logical output tree %q", node.ID, output.Tree)
			}
			if !actionPlanStageOwnsOutputTree(node.Stage, output.Tree) {
				return fmt.Errorf("family %s node %s cannot write logical %s tree", node.Stage, node.ID, output.Tree)
			}
			if err := validatePlanRelativePath("family node logical output", output.Path); err != nil {
				return err
			}
			if err := validatePlanRelativePath("family node artifact output", actionPlanOutputArtifactPath(output)); err != nil {
				return err
			}
			if err := validatePlanRelativePath("family store output", familyStorePath(node.ID, slot)); err != nil {
				return err
			}
		}
		nodes[node.ID] = node
	}
	inputSets, _, err := validateAndEncodeActionPlanInputSets(
		&ActionPlan{InputSets: f.InputSets, Nodes: f.Nodes}, nodes, sources, false,
	)
	if err != nil {
		return fmt.Errorf("family input sets: %w", err)
	}
	if !semanticIDsAlreadyValidated {
		if err := validateActionPlanAcyclic(nodes, inputSets); err != nil {
			return fmt.Errorf("family action graph: %w", err)
		}
	}
	if !semanticIDsAlreadyValidated {
		if stats != nil {
			stats.semanticNodesRehashed += len(f.Nodes)
			for _, node := range f.Nodes {
				stats.semanticSourceEdgesRehashed += len(node.Sources)
				stats.semanticInputEdgesRehashed += len(node.Inputs)
			}
		}
		ids, payloads, _, err := familyNodeIDs(&ActionPlan{Sources: f.Sources, InputSets: f.InputSets, Nodes: f.Nodes}, func(source ActionPlanSource) string { return source.ID })
		if err != nil {
			return err
		}
		seenPayloads := map[string][]byte{}
		for _, node := range f.Nodes {
			if ids[node.ID] != node.ID {
				return fmt.Errorf("family node ID %s does not match semantic content %s", node.ID, ids[node.ID])
			}
			if previous, ok := seenPayloads[node.ID]; ok && !bytes.Equal(previous, payloads[node.ID]) {
				return fmt.Errorf("family node semantic content ID collision %s", node.ID)
			}
			seenPayloads[node.ID] = payloads[node.ID]
		}
	}
	for _, node := range f.Nodes {
		for _, input := range node.Inputs {
			producer, ok := nodes[input.ProducerID]
			if !ok {
				return fmt.Errorf("family node %s references unknown producer %s", node.ID, input.ProducerID)
			}
			if input.Slot < 0 || input.Slot >= len(producer.Outputs) {
				return fmt.Errorf("family node %s references invalid producer slot %s[%d]", node.ID, input.ProducerID, input.Slot)
			}
		}
		memberships := f.Memberships[node.ID]
		if err := validateFamilyMembershipList("family node "+node.ID, memberships, f.Variants); err != nil {
			return err
		}
	}
	if len(f.Memberships) != len(f.Nodes) {
		return fmt.Errorf("family has %d membership records for %d nodes", len(f.Memberships), len(f.Nodes))
	}
	diagnosticPlan := &ActionPlan{Recipes: f.Recipes}
	for id, memberships := range f.preciseCompileMemberships {
		node, ok := nodes[id]
		if !ok {
			return fmt.Errorf("precise compiler diagnostic references unknown node %s", id)
		}
		if !typedFamilyCompilerNode(diagnosticPlan, node) {
			return fmt.Errorf("precise compiler diagnostic node %s is not a typed cc/cxx compile", id)
		}
		if err := validateFamilyMembershipList("precise compiler diagnostic node "+id, memberships, f.Variants); err != nil {
			return err
		}
		if !slices.Equal(memberships, f.Memberships[id]) {
			return fmt.Errorf(
				"precise compiler diagnostic node %s memberships %q do not match execution memberships %q",
				id, memberships, f.Memberships[id],
			)
		}
	}

	viewOwners := map[string]string{}
	for _, view := range f.Views {
		if !seenVariantsSlice(f.Variants, view.Variant) || !familyViewTree(view.Tree) {
			return fmt.Errorf("invalid family view variant/tree %q/%q", view.Variant, view.Tree)
		}
		node, ok := nodes[view.NodeID]
		if !ok || view.Slot < 0 || view.Slot >= len(node.Outputs) {
			return fmt.Errorf("family view %s/%s references unknown output %s[%d]", view.Variant, view.Tree, view.NodeID, view.Slot)
		}
		output := node.Outputs[view.Slot]
		if output.Tree != view.Tree || actionPlanOutputArtifactPath(output) != view.ArtifactPath {
			return fmt.Errorf("family view %s/%s/%s disagrees with producer %s[%d]", view.Variant, view.Tree, view.ArtifactPath, view.NodeID, view.Slot)
		}
		if !slices.Contains(f.Memberships[view.NodeID], view.Variant) {
			return fmt.Errorf("family view variant %s is not a member of node %s", view.Variant, view.NodeID)
		}
		key := strings.Join([]string{view.Variant, view.Tree, view.ArtifactPath}, "\x00")
		if owner := viewOwners[key]; owner != "" {
			return fmt.Errorf("family variant view %s is owned by both %s and %s", key, owner, view.NodeID)
		}
		viewOwners[key] = view.NodeID
	}
	seenValidations := map[string]bool{}
	for _, validation := range f.Validations {
		if !seenVariantsSlice(f.Variants, validation.Variant) {
			return fmt.Errorf("family validation has unknown variant %q", validation.Variant)
		}
		node, ok := nodes[validation.NodeID]
		if !ok || validation.Slot < 0 || validation.Slot >= len(node.Outputs) {
			return fmt.Errorf("family validation %s references unknown output %s[%d]", validation.Variant, validation.NodeID, validation.Slot)
		}
		if !slices.Contains(f.Memberships[validation.NodeID], validation.Variant) {
			return fmt.Errorf("family validation variant %s is not a member of node %s", validation.Variant, validation.NodeID)
		}
		key := validation.Variant + "\x00" + validation.NodeID + "\x00" + planOrdinal(validation.Slot)
		if seenValidations[key] {
			return fmt.Errorf("family repeats validation root %s", key)
		}
		seenValidations[key] = true
	}
	seenProducts := map[string]bool{}
	for _, product := range f.Products {
		if !seenVariantsSlice(f.Variants, product.Variant) || !LinuxKernelPlanProducts[product.Name] || !LinuxKernelPlanTrees[product.Tree] {
			return fmt.Errorf("invalid family product %q/%q/%q", product.Variant, product.Name, product.Tree)
		}
		if err := validatePlanRelativePath("family product", product.Path); err != nil {
			return err
		}
		key := product.Variant + "\x00" + product.Name
		if seenProducts[key] {
			return fmt.Errorf("family repeats product %s for variant %s", product.Name, product.Variant)
		}
		seenProducts[key] = true
	}
	executionRoots, err := f.executedCutRoots()
	if err != nil {
		return err
	}
	reachableMemberships, err := reachableActionPlanFamilyMemberships(
		nodes, inputSets, f.Memberships, f.Variants, f.Views, f.Products, f.Validations, executionRoots,
	)
	if err != nil {
		return err
	}
	for _, node := range f.Nodes {
		if !slices.Equal(f.Memberships[node.ID], reachableMemberships[node.ID]) {
			return fmt.Errorf(
				"family node %s memberships %q do not match root-derived memberships %q",
				node.ID, f.Memberships[node.ID], reachableMemberships[node.ID],
			)
		}
	}
	return nil
}

func seenVariantsSlice(variants []string, value string) bool {
	index, found := slices.BinarySearch(variants, value)
	return found && index >= 0
}

func familyNodeSourceProjections(node ActionPlanNode) (ActionPlanSourceProjections, error) {
	manifest := ActionPlanSourceProjections{
		Schema:   LinuxKernelSourceProjectionsSchema,
		Bindings: make(map[string]ActionPlanSourceProjectionBinding, len(node.familySourceProjections)),
	}
	for _, projection := range node.familySourceProjections {
		if projection.SourceOrdinal < 0 || projection.SourceOrdinal >= len(node.Sources) {
			return ActionPlanSourceProjections{}, fmt.Errorf(
				"family node %s source projection has invalid source ordinal %d",
				node.ID, projection.SourceOrdinal,
			)
		}
		edge := node.Sources[projection.SourceOrdinal]
		binding := edge.Role + ":" + planOrdinal(projection.SourceOrdinal)
		manifest.Bindings[binding] = ActionPlanSourceProjectionBinding{
			Tree: projection.Tree, Path: projection.Path,
		}
	}
	if err := manifest.Validate(); err != nil {
		return ActionPlanSourceProjections{}, fmt.Errorf("family node %s source projections: %w", node.ID, err)
	}
	return manifest, nil
}

// actionPlanPackedSourceProjectionMarkerFilenames encodes source-edge ordinals
// using the same bounded chunk/count/filename limits as packed generated-node
// inputs. Grammar: <eight-digit chunk>.<comma-separated lowercase-base36
// source ordinals>. The tree name is a parent marker component.
func actionPlanPackedSourceProjectionMarkerFilenames(ordinals []int) ([]string, error) {
	filenames := []string{}
	for cursor := 0; cursor < len(ordinals); {
		chunk := len(filenames)
		if chunk > maximumActionPlanOrdinal {
			return nil, fmt.Errorf("packed source-projection chunk index %d is out of range", chunk)
		}
		var filename strings.Builder
		filename.WriteString(planOrdinal(chunk))
		filename.WriteByte('.')
		count := 0
		for cursor < len(ordinals) && count < maximumActionPlanInputTuplesPerMarker {
			ordinal := ordinals[cursor]
			if ordinal < 0 || ordinal > maximumActionPlanOrdinal {
				return nil, fmt.Errorf("source projection ordinal %d is out of range", ordinal)
			}
			encoded := strconv.FormatInt(int64(ordinal), 36)
			separator := 0
			if count != 0 {
				separator = 1
			}
			if filename.Len()+separator+len(encoded) > maximumActionPlanInputMarkerFilename {
				if count == 0 {
					return nil, fmt.Errorf("packed source-projection ordinal %q exceeds %d-byte marker filename", encoded, maximumActionPlanInputMarkerFilename)
				}
				break
			}
			if separator != 0 {
				filename.WriteByte(',')
			}
			filename.WriteString(encoded)
			cursor++
			count++
		}
		filenames = append(filenames, filename.String())
	}
	return filenames, nil
}

func actionPlanFamilySourceProjectionEntries(node ActionPlanNode, root string) ([]actionPlanEntry, error) {
	if len(node.familySourceProjections) == 0 {
		return nil, nil
	}
	manifest, err := familyNodeSourceProjections(node)
	if err != nil {
		return nil, err
	}
	data, err := manifest.CanonicalJSON()
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	entries := []actionPlanEntry{{
		path: path.Join(root, "in", "source-tree-bindings", hex.EncodeToString(digest[:])+".json"),
		data: data,
	}}
	byTree := map[string][]int{}
	for _, projection := range node.familySourceProjections {
		byTree[projection.Tree] = append(byTree[projection.Tree], projection.SourceOrdinal)
	}
	for _, tree := range slices.Sorted(maps.Keys(byTree)) {
		ordinals := byTree[tree]
		sort.Ints(ordinals)
		filenames, err := actionPlanPackedSourceProjectionMarkerFilenames(ordinals)
		if err != nil {
			return nil, fmt.Errorf("family node %s source projection tree %q: %w", node.ID, tree, err)
		}
		for _, filename := range filenames {
			entries = append(entries, actionPlanEntry{
				path: path.Join(root, "in", "source-tree-pack", tree, filename),
			})
		}
	}
	return entries, nil
}

func familyNodeInputBindings(node ActionPlanNode, nodes map[string]ActionPlanNode) (ActionPlanInputBindings, error) {
	bindings := ActionPlanInputBindings{Schema: LinuxKernelInputBindingsSchema, Bindings: map[string]ActionPlanInputBinding{}}
	for ordinal, input := range node.Inputs {
		producer, ok := nodes[input.ProducerID]
		if !ok || input.Slot < 0 || input.Slot >= len(producer.Outputs) {
			return ActionPlanInputBindings{}, fmt.Errorf("family node %s has invalid input %s[%d]", node.ID, input.ProducerID, input.Slot)
		}
		bindings.Bindings[input.Role+":"+planOrdinal(ordinal)] = ActionPlanInputBinding{
			// A map_directory action template has exactly one execution
			// platform. Family execution is therefore split at host/target stage
			// boundaries, and each logical output tree is also the typed
			// TreeArtifact handoff between those templates. The leaf remains
			// content-addressed; ProjectionPath restores Kbuild's logical path in
			// the action-private view.
			Tree: producer.Outputs[input.Slot].Tree, Path: familyStorePath(producer.ID, input.Slot),
			ProjectionTree: producer.Outputs[input.Slot].Tree,
			ProjectionPath: actionPlanOutputArtifactPath(producer.Outputs[input.Slot]),
		}
	}
	return bindings, nil
}

func (f *ActionPlanFamily) entries() ([]actionPlanEntry, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	return f.entriesValidated()
}

// entriesValidated emits markers for a family whose representation has not
// escaped since its successful validation.
func (f *ActionPlanFamily) entriesValidated() ([]actionPlanEntry, error) {
	nodeOrdinals, indexedNodeIDs, err := actionPlanNodeOrdinalIndex(f.Nodes)
	if err != nil {
		return nil, err
	}
	entries := []actionPlanEntry{{path: path.Join("schema", LinuxKernelFamilyPlanSchema)}}
	for ordinal, nodeID := range indexedNodeIDs {
		entries = append(entries, actionPlanEntry{path: path.Join("index", planOrdinal(ordinal), nodeID)})
	}
	for scope, identity := range f.Toolsets {
		entries = append(entries, actionPlanEntry{path: path.Join("toolsets", scope, identity)})
	}
	for _, source := range f.Sources {
		entries = append(entries, actionPlanEntry{path: path.Join("sources", source.ID, source.Namespace, source.Path)})
	}
	for id, recipe := range f.Recipes {
		data, err := recipe.CanonicalJSON()
		if err != nil {
			return nil, err
		}
		entries = append(entries, actionPlanEntry{path: path.Join("recipes", id+".json"), data: data})
	}
	for digest, files := range f.Capsules {
		for pathname, contents := range files {
			entries = append(entries, actionPlanEntry{path: path.Join("capsules", digest, pathname), data: []byte(contents)})
		}
	}
	nodes := make(map[string]ActionPlanNode, len(f.Nodes))
	for _, node := range f.Nodes {
		nodes[node.ID] = node
	}
	sources := make(map[string]ActionPlanSource, len(f.Sources))
	for _, source := range f.Sources {
		sources[source.ID] = source
	}
	_, inputSetEntries, err := validateAndEncodeActionPlanInputSets(
		&ActionPlan{InputSets: f.InputSets, Nodes: f.Nodes}, nodes, sources, true,
	)
	if err != nil {
		return nil, fmt.Errorf("family input sets: %w", err)
	}
	entries = append(entries, inputSetEntries...)
	// Bindings may point at a producer whose digest sorts after its consumer.
	// Populate the complete lookup before emitting any node manifests.
	for _, node := range f.Nodes {
		root := path.Join("nodes", node.Stage, node.ID)
		entries = append(entries,
			actionPlanEntry{path: path.Join(root, "kind", node.Kind)},
			actionPlanEntry{path: path.Join(root, "recipe", node.Recipe)},
			actionPlanEntry{path: path.Join(root, "tool", node.Tool)},
			actionPlanEntry{path: path.Join(root, "product", node.Product)},
		)
		for ordinal, source := range node.Sources {
			entries = append(entries, actionPlanEntry{path: path.Join(root, "in", "source", source.Role, planOrdinal(ordinal), source.SourceID)})
		}
		if node.InputSet != "" {
			entries = append(entries, actionPlanEntry{path: path.Join(root, "in", "input-set", node.InputSet)})
		}
		sourceProjectionEntries, err := actionPlanFamilySourceProjectionEntries(node, root)
		if err != nil {
			return nil, err
		}
		entries = append(entries, sourceProjectionEntries...)
		inputEntries, err := actionPlanPackedInputEntries(node, root, nodeOrdinals)
		if err != nil {
			return nil, err
		}
		entries = append(entries, inputEntries...)
		for _, tree := range node.Trees {
			entries = append(entries, actionPlanEntry{path: path.Join(root, "in", "tree", tree)})
		}
		primaryScope := "target"
		if node.Stage == "prehost" || node.Stage == "host" {
			primaryScope = "host"
		}
		for _, tool := range node.AuxiliaryTools {
			scope, role, scoped, valid := toolaction.SplitBinding(tool)
			if !valid {
				return nil, fmt.Errorf("family node %s has invalid auxiliary tool %q", node.ID, tool)
			}
			form := "scoped"
			if !scoped {
				scope, form = primaryScope, "unscoped"
			}
			entries = append(entries, actionPlanEntry{path: path.Join(root, "in", "tool", scope, role, form)})
		}
		scopes, err := actionRecipeToolsetScopes(f.Recipes[node.Recipe])
		if err != nil {
			return nil, err
		}
		for _, scope := range scopes {
			entries = append(entries, actionPlanEntry{path: path.Join(root, "in", "toolset", scope)})
		}
		bindings, err := familyNodeInputBindings(node, nodes)
		if err != nil {
			return nil, err
		}
		bindingData, err := bindings.CanonicalJSON()
		if err != nil {
			return nil, err
		}
		bindingDigest := sha256.Sum256(bindingData)
		entries = append(entries, actionPlanEntry{
			path: path.Join(root, "in", "bindings", hex.EncodeToString(bindingDigest[:])+".json"), data: bindingData,
		})
		for slot, output := range node.Outputs {
			entries = append(entries, actionPlanEntry{path: path.Join(
				root, "out", output.Tree, planOrdinal(slot), "at", actionPlanOutputArtifactPath(output),
			)})
		}
	}
	for _, view := range f.Views {
		entries = append(entries, actionPlanEntry{path: path.Join(
			"variants", view.Variant, "view", view.Tree, "from", view.NodeID, planOrdinal(view.Slot), "at", view.ArtifactPath,
		)})
	}
	for _, validation := range f.Validations {
		entries = append(entries, actionPlanEntry{path: path.Join(
			"variants", validation.Variant, "validation", "from", validation.NodeID, planOrdinal(validation.Slot),
		)})
	}
	for _, product := range f.Products {
		entries = append(entries, actionPlanEntry{path: path.Join(
			"variants", product.Variant, "products", product.Name, "root", product.Tree, product.Path,
		)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	for index := 1; index < len(entries); index++ {
		if entries[index-1].path == entries[index].path {
			return nil, fmt.Errorf("family plan repeats marker %q", entries[index].path)
		}
	}
	return entries, nil
}

// familyPriorOutputMarker returns the compact descriptor carried by a segment
// when one of its nodes consumes an output produced in an earlier segment.
//
// Grammar:
//
//	prior/<producer global ordinal>/<slot>/<tree>/at/<artifact path>
//
// Both ordinals are fixed-width decimal values. The producer ID is recovered
// from the complete lexical index present in every shard; the physical store
// leaf remains derivable as nodes/<producer ID>/<slot>. The marker therefore
// retains exactly the logical tree/path information needed to reconstruct an
// input projection, without retaining the earlier node's recipe or body.
func familyPriorOutputMarker(
	nodeOrdinals map[string]int,
	producer ActionPlanNode,
	slot int,
) (actionPlanEntry, error) {
	ordinal, ok := nodeOrdinals[producer.ID]
	if !ok {
		return actionPlanEntry{}, fmt.Errorf("family prior output references unindexed producer %s", producer.ID)
	}
	if slot < 0 || slot >= len(producer.Outputs) {
		return actionPlanEntry{}, fmt.Errorf(
			"family prior output references invalid producer slot %s[%d]",
			producer.ID, slot,
		)
	}
	output := producer.Outputs[slot]
	return actionPlanEntry{path: path.Join(
		"prior", planOrdinal(ordinal), planOrdinal(slot), output.Tree, "at", actionPlanOutputArtifactPath(output),
	)}, nil
}

func validateFamilyPlanSegmentOutputs(outputDirs map[string]string) error {
	for segment := range outputDirs {
		if !LinuxKernelFamilyPlanSegments[segment] {
			return fmt.Errorf("kernel action plan family has unknown segment output %q", segment)
		}
	}
	if len(outputDirs) != len(linuxKernelFamilyPlanSegmentOrder) {
		return fmt.Errorf(
			"kernel action plan family requires exactly %d segment outputs, got %d",
			len(linuxKernelFamilyPlanSegmentOrder), len(outputDirs),
		)
	}
	seenOutputs := map[string]string{}
	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		output, ok := outputDirs[segment]
		if !ok || strings.TrimSpace(output) == "" {
			return fmt.Errorf("kernel action plan family has no %s segment output", segment)
		}
		canonical := filepath.Clean(output)
		if owner := seenOutputs[canonical]; owner != "" {
			return fmt.Errorf(
				"kernel action plan family segments %s and %s share output directory %q",
				owner, segment, canonical,
			)
		}
		seenOutputs[canonical] = segment
	}
	return nil
}

// segmentEntries produces four independently parseable family-plan marker
// trees. Every shard keeps the schema, complete lexical node index, and both
// toolset identities. Action bodies and their recipe/source/capsule payloads
// occur only in the segment which executes them. Variant views and products
// occur only in the terminal target shard.
func (f *ActionPlanFamily) segmentEntries() (map[string][]actionPlanEntry, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	return f.segmentEntriesValidated()
}

func (f *ActionPlanFamily) segmentEntriesValidated() (map[string][]actionPlanEntry, error) {
	entries, err := f.entriesValidated()
	if err != nil {
		return nil, err
	}
	nodeOrdinals, _, err := actionPlanNodeOrdinalIndex(f.Nodes)
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]ActionPlanNode, len(f.Nodes))
	nodeSegments := make(map[string]string, len(f.Nodes))
	sources := make(map[string]ActionPlanSource, len(f.Sources))
	for _, source := range f.Sources {
		sources[source.ID] = source
	}

	recipeSegments := map[string]map[string]bool{}
	sourceSegments := map[string]map[string]bool{}
	capsuleSegments := map[string]map[string]bool{}
	inputSetSegments := map[string]map[string]bool{}
	inputSetRootsBySegment := map[string][]string{}
	inputSetNodesBySegment := map[string]map[string]ActionPlanInputSetNode{}
	addReference := func(index map[string]map[string]bool, id, segment string) {
		segments := index[id]
		if segments == nil {
			segments = map[string]bool{}
			index[id] = segments
		}
		segments[segment] = true
	}
	inputSets, err := NewActionPlanInputSetStoreFromNodes(f.InputSets)
	if err != nil {
		return nil, fmt.Errorf("family input sets: %w", err)
	}
	for _, node := range f.Nodes {
		segment, ok := familyPlanSegmentForStage(node.Stage)
		if !ok {
			return nil, fmt.Errorf("family node %s has no execution segment for stage %q", node.ID, node.Stage)
		}
		nodes[node.ID] = node
		nodeSegments[node.ID] = segment
		addReference(recipeSegments, node.Recipe, segment)
		for _, edge := range node.Sources {
			addReference(sourceSegments, edge.SourceID, segment)
			source := sources[edge.SourceID]
			if source.Namespace == "capsule" {
				addReference(capsuleSegments, source.Path, segment)
			}
		}
		inputSetRootsBySegment[segment] = append(inputSetRootsBySegment[segment], node.InputSet)
	}
	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		closure, err := inputSets.ReachableNodesForRoots(inputSetRootsBySegment[segment])
		if err != nil {
			return nil, fmt.Errorf("family %s input sets: %w", segment, err)
		}
		inputSetNodesBySegment[segment] = closure
		for id := range closure {
			addReference(inputSetSegments, id, segment)
		}
		for _, inputSetNode := range closure {
			for _, entry := range inputSetNode.Entries {
				if entry.SourceID == "" {
					continue
				}
				addReference(sourceSegments, entry.SourceID, segment)
				source, ok := sources[entry.SourceID]
				if !ok {
					return nil, fmt.Errorf("family %s input set references unknown source %s", segment, entry.SourceID)
				}
				if source.Namespace == "capsule" {
					addReference(capsuleSegments, source.Path, segment)
				}
			}
		}
	}

	segments := make(map[string][]actionPlanEntry, len(linuxKernelFamilyPlanSegmentOrder))
	appendEntry := func(segment string, entry actionPlanEntry) {
		segments[segment] = append(segments[segment], entry)
	}
	appendAll := func(entry actionPlanEntry) {
		for _, segment := range linuxKernelFamilyPlanSegmentOrder {
			appendEntry(segment, entry)
		}
	}
	for _, entry := range entries {
		parts := strings.Split(entry.path, "/")
		switch parts[0] {
		case "schema", "toolsets", "index":
			appendAll(entry)
		case "recipes":
			id := strings.TrimSuffix(parts[1], ".json")
			for segment := range recipeSegments[id] {
				appendEntry(segment, entry)
			}
		case "sources":
			for segment := range sourceSegments[parts[1]] {
				appendEntry(segment, entry)
			}
		case "capsules":
			canonical := strings.Join(parts[1:], "/")
			for segment := range capsuleSegments[canonical] {
				appendEntry(segment, entry)
			}
		case "input-sets":
			if len(parts) < 2 {
				return nil, fmt.Errorf("family plan cannot shard input-set marker %q", entry.path)
			}
			for segment := range inputSetSegments[parts[1]] {
				appendEntry(segment, entry)
			}
		case "nodes":
			segment, ok := familyPlanSegmentForStage(parts[1])
			if !ok {
				return nil, fmt.Errorf("family plan cannot shard node marker %q", entry.path)
			}
			appendEntry(segment, entry)
		case "variants":
			appendEntry("target", entry)
		default:
			return nil, fmt.Errorf("family plan cannot shard marker %q", entry.path)
		}
	}

	// Input packs refer to the complete lexical node index. For a producer
	// outside the current segment, add only its typed output descriptor. This
	// is the sole prior-node metadata retained in a consumer shard.
	seenPrior := map[string]map[string]bool{}
	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		seenPrior[segment] = map[string]bool{}
	}
	for _, node := range f.Nodes {
		consumerSegment := nodeSegments[node.ID]
		for _, input := range node.Inputs {
			producer, ok := nodes[input.ProducerID]
			if !ok {
				return nil, fmt.Errorf("family node %s references unknown producer %s", node.ID, input.ProducerID)
			}
			if familyStageOrdinal(producer.Stage) > familyStageOrdinal(node.Stage) {
				return nil, fmt.Errorf(
					"family node %s in %s stage has backward dependency on %s node %s",
					node.ID, node.Stage, producer.Stage, producer.ID,
				)
			}
			if nodeSegments[producer.ID] == consumerSegment {
				continue
			}
			descriptor, err := familyPriorOutputMarker(nodeOrdinals, producer, input.Slot)
			if err != nil {
				return nil, err
			}
			if !seenPrior[consumerSegment][descriptor.path] {
				appendEntry(consumerSegment, descriptor)
				seenPrior[consumerSegment][descriptor.path] = true
			}
		}
	}
	for _, consumerSegment := range linuxKernelFamilyPlanSegmentOrder {
		for _, inputSetNode := range inputSetNodesBySegment[consumerSegment] {
			for _, entry := range inputSetNode.Entries {
				if entry.ProducerID == "" {
					continue
				}
				producer, ok := nodes[entry.ProducerID]
				if !ok {
					return nil, fmt.Errorf("family %s input set references unknown producer %s", consumerSegment, entry.ProducerID)
				}
				if nodeSegments[producer.ID] == consumerSegment {
					continue
				}
				descriptor, err := familyPriorOutputMarker(nodeOrdinals, producer, entry.Slot)
				if err != nil {
					return nil, err
				}
				if !seenPrior[consumerSegment][descriptor.path] {
					appendEntry(consumerSegment, descriptor)
					seenPrior[consumerSegment][descriptor.path] = true
				}
			}
		}
	}
	for _, validation := range f.Validations {
		producer, ok := nodes[validation.NodeID]
		if !ok {
			return nil, fmt.Errorf("family validation references unknown producer %s", validation.NodeID)
		}
		const consumerSegment = "target"
		if nodeSegments[producer.ID] == consumerSegment {
			continue
		}
		descriptor, err := familyPriorOutputMarker(nodeOrdinals, producer, validation.Slot)
		if err != nil {
			return nil, err
		}
		if !seenPrior[consumerSegment][descriptor.path] {
			appendEntry(consumerSegment, descriptor)
			seenPrior[consumerSegment][descriptor.path] = true
		}
	}

	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		sort.Slice(segments[segment], func(i, j int) bool {
			return segments[segment][i].path < segments[segment][j].path
		})
		for index := 1; index < len(segments[segment]); index++ {
			if segments[segment][index-1].path == segments[segment][index].path {
				return nil, fmt.Errorf(
					"family %s segment repeats marker %q",
					segment, segments[segment][index].path,
				)
			}
		}
	}
	return segments, nil
}

func (f *ActionPlanFamily) Write(outputDirectory string) error {
	entries, err := f.entries()
	if err != nil {
		return err
	}
	return writeActionPlanTree(outputDirectory, entries)
}

// WriteSegments writes exactly one self-contained marker tree for each family
// execution segment. It never publishes a complete family plan alongside the
// shards.
func (f *ActionPlanFamily) WriteSegments(outputDirs map[string]string) error {
	if err := validateFamilyPlanSegmentOutputs(outputDirs); err != nil {
		return err
	}
	entries, err := f.segmentEntries()
	if err != nil {
		return err
	}
	return writeActionPlanFamilySegments(outputDirs, entries)
}

func writeActionPlanFamilySegments(outputDirs map[string]string, entries map[string][]actionPlanEntry) error {
	for _, segment := range linuxKernelFamilyPlanSegmentOrder {
		if err := writeActionPlanTree(outputDirs[segment], entries[segment]); err != nil {
			return fmt.Errorf("write %s action-plan family segment: %w", segment, err)
		}
	}
	return nil
}

func basisPoints(numerator, denominator int) int {
	if denominator == 0 {
		return 0
	}
	return numerator * 10000 / denominator
}

func sortedReuseKinds(counts map[string]int) []ActionPlanFamilyReuseKind {
	kinds := make([]ActionPlanFamilyReuseKind, 0, len(counts))
	for kind, count := range counts {
		kinds = append(kinds, ActionPlanFamilyReuseKind{Kind: kind, Nodes: count})
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i].Kind < kinds[j].Kind })
	return kinds
}

func (f *ActionPlanFamily) ReuseReport() (ActionPlanFamilyReuseReport, error) {
	if err := f.validate(); err != nil {
		return ActionPlanFamilyReuseReport{}, err
	}
	return f.reuseReportValidated()
}

func (f *ActionPlanFamily) reuseReportValidated() (ActionPlanFamilyReuseReport, error) {
	for _, scope := range []string{"host", "target"} {
		if f.Toolsets[scope] == "" {
			return ActionPlanFamilyReuseReport{}, fmt.Errorf("family reuse report has no %s toolset identity", scope)
		}
	}
	report := ActionPlanFamilyReuseReport{
		Schema:                 LinuxKernelFamilyReuseReportSchema,
		Toolsets:               maps.Clone(f.Toolsets),
		Nodes:                  make([]ActionPlanFamilyReuseNode, 0, len(f.Nodes)),
		Variants:               make([]ActionPlanFamilyReuseVariant, 0, len(f.Variants)),
		MembershipGroups:       []ActionPlanFamilyReuseGroup{},
		Pairs:                  []ActionPlanFamilyReusePair{},
		UniqueNodes:            len(f.Nodes),
		OpaqueReasons:          []ActionPlanFamilyOpaqueReason{},
		PreciseCompiles:        []ActionPlanFamilyPreciseCompile{},
		ObservedHeaderFrontier: cloneActionPlanFamilyObservedHeaderFrontier(f.observedHeaderFrontier),
	}
	diagnosticPlan := &ActionPlan{Recipes: f.Recipes}
	nodes := make(map[string]ActionPlanNode, len(f.Nodes))
	for _, node := range f.Nodes {
		nodes[node.ID] = node
		members := f.Memberships[node.ID]
		_, precise := f.preciseCompileMemberships[node.ID]
		report.Nodes = append(report.Nodes, ActionPlanFamilyReuseNode{
			NodeID: node.ID, Kind: node.Kind, Memberships: slices.Clone(members),
			TypedCompiler: typedFamilyCompilerNode(diagnosticPlan, node), PreciseCompile: precise,
		})
		report.VariantInstances += len(members)
		if len(members) > 1 {
			report.SharedNodes++
		}
	}
	report.ReusedInstances = report.VariantInstances - report.UniqueNodes
	for _, variant := range f.Variants {
		entry := ActionPlanFamilyReuseVariant{Name: variant}
		kinds := map[string]int{}
		for _, node := range f.Nodes {
			members := f.Memberships[node.ID]
			if !slices.Contains(members, variant) {
				continue
			}
			entry.Nodes++
			kinds[node.Kind]++
			if typedFamilyCompilerNode(diagnosticPlan, node) {
				entry.TypedCompilerNodes++
			}
			if len(members) > 1 {
				entry.SharedNodes++
			} else {
				entry.ExclusiveNodes++
			}
		}
		entry.Kinds = sortedReuseKinds(kinds)
		for _, members := range f.preciseCompileMemberships {
			if slices.Contains(members, variant) {
				entry.PreciseCompileNodes++
			}
		}
		entry.PreciseCompilerCoverageBasisPoints = basisPoints(entry.PreciseCompileNodes, entry.TypedCompilerNodes)
		report.Variants = append(report.Variants, entry)
	}
	for _, id := range slices.Sorted(maps.Keys(f.preciseCompileMemberships)) {
		node, ok := nodes[id]
		if !ok {
			return ActionPlanFamilyReuseReport{}, fmt.Errorf("precise compiler diagnostic references unknown node %s", id)
		}
		outputs := make([]string, 0, len(node.Outputs))
		outputDetails := make([]ActionPlanFamilyPreciseCompileOutput, 0, len(node.Outputs))
		for slot, output := range node.Outputs {
			outputs = append(outputs, output.Tree+":"+output.Path)
			outputDetails = append(outputDetails, ActionPlanFamilyPreciseCompileOutput{
				Slot: slot, Tree: output.Tree, LogicalPath: output.Path,
				ArtifactPath: actionPlanOutputArtifactPath(output), StorePath: familyStorePath(id, slot),
			})
		}
		report.PreciseCompiles = append(report.PreciseCompiles, ActionPlanFamilyPreciseCompile{
			NodeID: id, Outputs: outputs, OutputDetails: outputDetails,
			Memberships: slices.Clone(f.preciseCompileMemberships[id]),
		})
	}
	for _, variant := range f.Variants {
		for _, key := range slices.Sorted(maps.Keys(f.opaqueReasons[variant])) {
			kind, reason, ok := strings.Cut(key, "\x00")
			if !ok {
				continue
			}
			report.OpaqueReasons = append(report.OpaqueReasons, ActionPlanFamilyOpaqueReason{
				Variant: variant, Kind: kind, Reason: reason, Nodes: f.opaqueReasons[variant][key],
			})
		}
	}
	type groupState struct {
		variants []string
		count    int
		kinds    map[string]int
	}
	groups := map[string]*groupState{}
	for _, node := range f.Nodes {
		members := f.Memberships[node.ID]
		key := strings.Join(members, "\x00")
		group := groups[key]
		if group == nil {
			group = &groupState{variants: slices.Clone(members), kinds: map[string]int{}}
			groups[key] = group
		}
		group.count++
		group.kinds[node.Kind]++
	}
	groupKeys := slices.Sorted(maps.Keys(groups))
	for _, key := range groupKeys {
		group := groups[key]
		report.MembershipGroups = append(report.MembershipGroups, ActionPlanFamilyReuseGroup{
			Variants: group.variants, Nodes: group.count, Kinds: sortedReuseKinds(group.kinds),
		})
	}
	variantCounts := map[string]int{}
	eligibleCounts := map[string]int{}
	typedCompilerCounts := map[string]int{}
	preciseCompileCounts := map[string]int{}
	for _, variant := range report.Variants {
		variantCounts[variant.Name] = variant.Nodes
		typedCompilerCounts[variant.Name] = variant.TypedCompilerNodes
		preciseCompileCounts[variant.Name] = variant.PreciseCompileNodes
		for _, kind := range variant.Kinds {
			if kind.Kind == "compile" || kind.Kind == "archive" {
				eligibleCounts[variant.Name] += kind.Nodes
			}
		}
	}
	for leftIndex, left := range f.Variants {
		for _, right := range f.Variants[leftIndex+1:] {
			shared, eligibleShared, typedCompilerShared, preciseCompileShared := 0, 0, 0, 0
			for _, node := range f.Nodes {
				members := f.Memberships[node.ID]
				if slices.Contains(members, left) && slices.Contains(members, right) {
					shared++
					if node.Kind == "compile" || node.Kind == "archive" {
						eligibleShared++
					}
					if typedFamilyCompilerNode(diagnosticPlan, node) {
						typedCompilerShared++
					}
				}
			}
			for _, members := range f.preciseCompileMemberships {
				if slices.Contains(members, left) && slices.Contains(members, right) {
					preciseCompileShared++
				}
			}
			report.Pairs = append(report.Pairs, ActionPlanFamilyReusePair{
				Left: left, Right: right,
				LeftNodes: variantCounts[left], RightNodes: variantCounts[right], SharedNodes: shared,
				LeftReuseBasisPoints:  basisPoints(shared, variantCounts[left]),
				RightReuseBasisPoints: basisPoints(shared, variantCounts[right]),
				EligibleLeftNodes:     eligibleCounts[left], EligibleRightNodes: eligibleCounts[right], EligibleSharedNodes: eligibleShared,
				EligibleLeftReuseBasisPoints:  basisPoints(eligibleShared, eligibleCounts[left]),
				EligibleRightReuseBasisPoints: basisPoints(eligibleShared, eligibleCounts[right]),
				TypedLeftCompilerNodes:        typedCompilerCounts[left], TypedRightCompilerNodes: typedCompilerCounts[right],
				TypedSharedCompilerNodes:   typedCompilerShared,
				TypedLeftReuseBasisPoints:  basisPoints(typedCompilerShared, typedCompilerCounts[left]),
				TypedRightReuseBasisPoints: basisPoints(typedCompilerShared, typedCompilerCounts[right]),
				PreciseLeftCompileNodes:    preciseCompileCounts[left], PreciseRightCompileNodes: preciseCompileCounts[right],
				PreciseSharedCompileNodes:             preciseCompileShared,
				PreciseLeftReuseBasisPoints:           basisPoints(preciseCompileShared, preciseCompileCounts[left]),
				PreciseRightReuseBasisPoints:          basisPoints(preciseCompileShared, preciseCompileCounts[right]),
				EffectivePreciseLeftReuseBasisPoints:  basisPoints(preciseCompileShared, typedCompilerCounts[left]),
				EffectivePreciseRightReuseBasisPoints: basisPoints(preciseCompileShared, typedCompilerCounts[right]),
			})
		}
	}
	return report, nil
}

func (f *ActionPlanFamily) WriteReuseReport(output string) error {
	report, err := f.ReuseReport()
	if err != nil {
		return err
	}
	return writeActionPlanFamilyReuseReport(output, report)
}

func writeActionPlanFamilyReuseReport(output string, report ActionPlanFamilyReuseReport) error {
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return writeCanonicalFile(output, append(data, '\n'))
}

func (validated *validatedActionPlanFamily) writeSegmentsAndReuseReport(
	outputDirs map[string]string,
	reuseReportOutput string,
) error {
	entries, err := validated.family.segmentEntriesValidated()
	if err != nil {
		return err
	}
	report, err := validated.family.reuseReportValidated()
	if err != nil {
		return err
	}
	reportData, err := json.Marshal(report)
	if err != nil {
		return err
	}
	// Prepare both representations before publishing either output. In
	// particular, a malformed diagnostic cannot leave a valid-looking set of
	// family shards without its matching report.
	if err := writeActionPlanFamilySegments(outputDirs, entries); err != nil {
		return err
	}
	return writeCanonicalFile(reuseReportOutput, append(reportData, '\n'))
}

// BuildAndWriteActionPlanFamily reduces, validates, and emits one family while
// the validated representation remains sealed inside this package. This is the
// efficient path for one-shot producers: unlike calling the standalone public
// emission methods in sequence, it does not revalidate the exported mutable
// family before each output.
func BuildAndWriteActionPlanFamily(
	variants []ValidatedActionPlanFamilyVariant,
	segmentOutputDirs map[string]string,
	reuseReportOutput string,
) error {
	// Clone before validation so the private emission interval does not retain a
	// caller-owned mutable map.
	outputs := maps.Clone(segmentOutputDirs)
	if err := validateFamilyPlanSegmentOutputs(outputs); err != nil {
		return err
	}
	if strings.TrimSpace(reuseReportOutput) == "" {
		return fmt.Errorf("output filename must not be empty")
	}
	inputs := make([]actionPlanFamilyBuildVariant, 0, len(variants))
	for index, variant := range variants {
		if !variant.validated {
			return fmt.Errorf("family variant %d was not returned by ReadActionPlanFamilyVariant", index)
		}
		inputs = append(inputs, actionPlanFamilyBuildVariant{
			variant: ActionPlanFamilyVariant{
				Name: variant.name, Snapshot: variant.snapshot,
			},
			snapshotValidated: true,
		})
	}
	validated, err := buildValidatedActionPlanFamily(inputs)
	if err != nil {
		return err
	}
	return validated.writeSegmentsAndReuseReport(outputs, reuseReportOutput)
}

func writeCanonicalFile(filename string, data []byte) error {
	if strings.TrimSpace(filename) == "" {
		return fmt.Errorf("output filename must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filename, data, 0o644)
}
