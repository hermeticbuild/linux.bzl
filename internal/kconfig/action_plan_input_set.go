package kconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ActionPlanInputSetSchemaVersion identifies the canonical serialized form of
// an action-plan input set. Node IDs are the SHA-256 digest of this form.
const ActionPlanInputSetSchemaVersion = "linux-kernel-input-set-v1"

const (
	actionPlanInputSetLeafCapacity  = 16
	actionPlanInputSetDigestNibbles = sha256.Size * 2
	// Match the working-tree materialized-core entry bound. Retaining only
	// root IDs bounds this cache independently of transitive input counts.
	maximumActionPlanInputSetUnionCachePairs = 1 << 15
)

// ActionPlanInputSetTargetKind distinguishes the namespaces in which an input
// is materialized. A target is the identity key of an input-set entry.
type ActionPlanInputSetTargetKind string

const (
	ActionPlanInputSetWorkTarget    ActionPlanInputSetTargetKind = "work"
	ActionPlanInputSetTreeTarget    ActionPlanInputSetTargetKind = "tree"
	ActionPlanInputSetAmbientTarget ActionPlanInputSetTargetKind = "ambient"
)

// ActionPlanInputSetTarget names one materialized input. Tree is set only for
// tree targets; Path is relative to the selected namespace.
type ActionPlanInputSetTarget struct {
	Kind ActionPlanInputSetTargetKind `json:"kind"`
	Tree string                       `json:"tree,omitempty"`
	Path string                       `json:"path"`
}

// ActionPlanInputSetEntry records both an input's materialization target and
// its exact provenance. Exactly one of SourceID and ProducerID must be set.
// Slot is meaningful only for producer provenance.
type ActionPlanInputSetEntry struct {
	Target       ActionPlanInputSetTarget `json:"target"`
	SourceID     string                   `json:"source_id,omitempty"`
	ProducerID   string                   `json:"producer_id,omitempty"`
	Slot         int                      `json:"slot,omitempty"`
	CompilerUse  bool                     `json:"compiler_use,omitempty"`
	AuxiliaryUse bool                     `json:"auxiliary_use,omitempty"`
}

// ActionPlanInputSetChild is one edge in a radix branch. Nibble is a single
// lower-case hexadecimal digit from the target-key SHA-256 digest.
type ActionPlanInputSetChild struct {
	Nibble string `json:"nibble"`
	ID     string `json:"id"`
}

// ActionPlanInputSetNodeKind domain-separates leaf and branch encodings.
type ActionPlanInputSetNodeKind string

const (
	ActionPlanInputSetLeafNode   ActionPlanInputSetNodeKind = "leaf"
	ActionPlanInputSetBranchNode ActionPlanInputSetNodeKind = "branch"
)

// ActionPlanInputSetNode is a canonical persistent radix node. A node is
// either a leaf (Entries is non-empty) or a branch (Children is non-empty).
// Empty sets are represented by the empty root ID and have no node.
type ActionPlanInputSetNode struct {
	Schema   string                     `json:"schema"`
	Kind     ActionPlanInputSetNodeKind `json:"kind"`
	Depth    int                        `json:"depth"`
	Count    int                        `json:"count"`
	Children []ActionPlanInputSetChild  `json:"children,omitempty"`
	Entries  []ActionPlanInputSetEntry  `json:"entries,omitempty"`
}

// ActionPlanInputSetConflictResolver resolves entries with the same target.
// The returned entry must retain target. A nil resolver accepts identical
// entries and rejects every other conflict.
type ActionPlanInputSetConflictResolver func(
	target ActionPlanInputSetTarget,
	left, right ActionPlanInputSetEntry,
) (ActionPlanInputSetEntry, error)

type actionPlanInputSetDigest func([]byte) string

// ActionPlanInputSetStore owns immutable, content-addressed nodes. It may hold
// more than one root and therefore naturally retains historical versions.
type ActionPlanInputSetStore struct {
	nodes                       map[string]ActionPlanInputSetNode
	witnesses                   map[string][]byte
	digest                      actionPlanInputSetDigest
	keyDigest                   func(ActionPlanInputSetTarget) [sha256.Size]byte
	allowProvisionalProducerIDs bool
	// Only standard immutable digest constructors enable resolver-independent
	// union caching. No resolver, callback event, or error is retained.
	unionCacheEnabled     bool
	unionCacheProvisional bool
	unionCache            map[[2]string]string
}

// ActionPlanInputSetTargetStableMapper applies one immutable, target-stable
// transformation across any number of roots. Persistent subtries are mapped
// once and shared by every result, so the cost follows unique input-set nodes
// rather than the sum of every consumer's transitive input count.
//
// A mapper is deliberately callback-scoped instead of store-scoped: callers
// must create a new mapper whenever the transform's semantic environment
// changes. It is sequential and is not safe for concurrent use.
type ActionPlanInputSetTargetStableMapper struct {
	store      *ActionPlanInputSetStore
	transform  func(ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error)
	mapSubtree func(string) (bool, error)
	mapped     map[string]string
}

// NewActionPlanInputSetStore returns an empty canonical input-set store.
func NewActionPlanInputSetStore() *ActionPlanInputSetStore {
	store := newActionPlanInputSetStoreWithDigest(actionPlanInputSetSHA256)
	store.unionCacheEnabled = true
	return store
}

func newPlanningActionPlanInputSetStore() *ActionPlanInputSetStore {
	store := NewActionPlanInputSetStore()
	store.allowProvisionalProducerIDs = true
	return store
}

func newActionPlanInputSetStoreWithDigest(digest actionPlanInputSetDigest) *ActionPlanInputSetStore {
	return newActionPlanInputSetStoreWithDigests(digest, actionPlanInputSetTargetDigest)
}

func newActionPlanInputSetStoreWithDigests(digest actionPlanInputSetDigest, keyDigest func(ActionPlanInputSetTarget) [sha256.Size]byte) *ActionPlanInputSetStore {
	return &ActionPlanInputSetStore{
		nodes:     make(map[string]ActionPlanInputSetNode),
		witnesses: make(map[string][]byte),
		digest:    digest,
		keyDigest: keyDigest,
	}
}

func actionPlanInputSetSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// NewActionPlanInputSetStoreFromNodes imports and validates serialized nodes.
// Multiple depth-zero nodes may coexist as independent roots.
func NewActionPlanInputSetStoreFromNodes(nodes map[string]ActionPlanInputSetNode) (*ActionPlanInputSetStore, error) {
	store := NewActionPlanInputSetStore()
	for id, node := range nodes {
		canonical, err := actionPlanInputSetCanonicalNode(node)
		if err != nil {
			return nil, fmt.Errorf("encode input-set node %q: %w", id, err)
		}
		if err := validatePlanDigest("input-set node ID", id); err != nil {
			return nil, err
		}
		if got := store.digest(canonical); got != id {
			return nil, fmt.Errorf("input-set node %q has canonical ID %q", id, got)
		}
		store.nodes[id] = cloneActionPlanInputSetNode(node)
		store.witnesses[id] = append([]byte(nil), canonical...)
	}
	if err := store.validateAllNodes(); err != nil {
		return nil, err
	}
	return store, nil
}

// NodeCount reports the number of unique nodes, including retained historical
// nodes, in the store.
func (s *ActionPlanInputSetStore) NodeCount() int {
	if s == nil {
		return 0
	}
	return len(s.nodes)
}

// Node returns a defensive copy of a node.
func (s *ActionPlanInputSetStore) Node(id string) (ActionPlanInputSetNode, bool) {
	if s == nil {
		return ActionPlanInputSetNode{}, false
	}
	node, ok := s.nodes[id]
	if !ok {
		return ActionPlanInputSetNode{}, false
	}
	return cloneActionPlanInputSetNode(node), true
}

// Nodes returns defensive copies of all nodes, suitable for serialization.
func (s *ActionPlanInputSetStore) Nodes() map[string]ActionPlanInputSetNode {
	result := make(map[string]ActionPlanInputSetNode, s.NodeCount())
	if s == nil {
		return result
	}
	for id, node := range s.nodes {
		result[id] = cloneActionPlanInputSetNode(node)
	}
	return result
}

// ReachableNodes returns only the immutable closure of root. This is the
// preferred serialization boundary because a persistent store also retains
// superseded roots and their now-unreachable nodes.
func (s *ActionPlanInputSetStore) ReachableNodes(root string) (map[string]ActionPlanInputSetNode, error) {
	return s.ReachableNodesForRoots([]string{root})
}

// ReachableNodesForRoots returns the union of the immutable closures of roots.
// Shared subtries are validated and copied once, independent of the number of
// consumer roots which reference them.
func (s *ActionPlanInputSetStore) ReachableNodesForRoots(roots []string) (map[string]ActionPlanInputSetNode, error) {
	result := make(map[string]ActionPlanInputSetNode)
	if err := s.ready(); err != nil {
		return nil, err
	}
	validated := make(map[string]string)
	visiting := make(map[string]bool)
	for _, root := range roots {
		if root == "" {
			continue
		}
		if err := s.validateRootReference(root); err != nil {
			return nil, err
		}
		if err := s.validateReachable(root, "", visiting, validated); err != nil {
			return nil, err
		}
	}
	var visit func(string) error
	visit = func(id string) error {
		if _, exists := result[id]; exists {
			return nil
		}
		node, err := s.nodeAt(id)
		if err != nil {
			return err
		}
		result[id] = cloneActionPlanInputSetNode(node)
		for _, child := range node.Children {
			if err := visit(child.ID); err != nil {
				return err
			}
		}
		return nil
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		if err := visit(root); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// CanonicalWitness returns the exact bytes whose digest is id. Retaining this
// witness makes an attempted content-ID collision observable instead of
// silently aliasing distinct nodes.
func (s *ActionPlanInputSetStore) CanonicalWitness(id string) ([]byte, bool) {
	if s == nil {
		return nil, false
	}
	witness, ok := s.witnesses[id]
	return append([]byte(nil), witness...), ok
}

// Insert adds entry or replaces the entry at the same target. Other roots and
// every untouched subtree remain valid and shared.
func (s *ActionPlanInputSetStore) Insert(root string, entry ActionPlanInputSetEntry) (string, error) {
	if err := s.ready(); err != nil {
		return "", err
	}
	if err := s.validateEntry(entry); err != nil {
		return "", err
	}
	if root == "" {
		return s.build(0, []ActionPlanInputSetEntry{entry})
	}
	if err := s.validateRootReference(root); err != nil {
		return "", err
	}
	return s.insertAt(root, entry)
}

// Replace replaces an existing target and rejects a missing target.
func (s *ActionPlanInputSetStore) Replace(root string, entry ActionPlanInputSetEntry) (string, error) {
	if err := s.validateEntry(entry); err != nil {
		return "", err
	}
	_, found, err := s.Lookup(root, entry.Target)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("replace input-set target %s: target does not exist", actionPlanInputSetTargetDescription(entry.Target))
	}
	return s.Insert(root, entry)
}

// Delete removes target. It reports whether a target was present.
func (s *ActionPlanInputSetStore) Delete(root string, target ActionPlanInputSetTarget) (string, bool, error) {
	if err := s.ready(); err != nil {
		return "", false, err
	}
	if err := validateActionPlanInputSetTarget(target); err != nil {
		return "", false, err
	}
	if root == "" {
		return "", false, nil
	}
	if err := s.validateRootReference(root); err != nil {
		return "", false, err
	}
	return s.deleteAt(root, target)
}

// Lookup finds target without materializing the complete set.
func (s *ActionPlanInputSetStore) Lookup(root string, target ActionPlanInputSetTarget) (ActionPlanInputSetEntry, bool, error) {
	if err := s.ready(); err != nil {
		return ActionPlanInputSetEntry{}, false, err
	}
	if err := validateActionPlanInputSetTarget(target); err != nil {
		return ActionPlanInputSetEntry{}, false, err
	}
	if root == "" {
		return ActionPlanInputSetEntry{}, false, nil
	}
	if err := s.validateRootReference(root); err != nil {
		return ActionPlanInputSetEntry{}, false, err
	}
	return s.lookupAt(root, target)
}

// Union returns the canonical union of left and right. It shares all subtrees
// that occur unchanged in either input.
func (s *ActionPlanInputSetStore) Union(left, right string, resolve ActionPlanInputSetConflictResolver) (string, error) {
	if err := s.ready(); err != nil {
		return "", err
	}
	if s.unionCacheEnabled && s.unionCacheProvisional != s.allowProvisionalProducerIDs {
		s.unionCache = nil
		s.unionCacheProvisional = s.allowProvisionalProducerIDs
	}
	if left == "" {
		if right != "" {
			if err := s.validateRootReference(right); err != nil {
				return "", err
			}
		}
		return right, nil
	}
	if right == "" {
		if err := s.validateRootReference(left); err != nil {
			return "", err
		}
		return left, nil
	}
	if err := s.validateRootReference(left); err != nil {
		return "", err
	}
	if err := s.validateRootReference(right); err != nil {
		return "", err
	}
	// Keep the original reference checks ahead of every hit, and leave
	// trivial identities and custom digest hooks on their original path.
	if left == right || !s.unionCacheEnabled {
		return s.unionAt(left, right, resolve)
	}
	key := [2]string{left, right}
	if root, ok := s.unionCache[key]; ok {
		return root, nil
	}
	if len(s.unionCache) >= maximumActionPlanInputSetUnionCachePairs {
		return s.unionAt(left, right, resolve)
	}
	invoked := false
	observedResolve := resolve
	if resolve != nil {
		observedResolve = func(target ActionPlanInputSetTarget, left, right ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
			invoked = true
			return resolve(target, left, right)
		}
	}
	// Nil must stay nil to preserve its original conflict diagnostic. Every
	// conflicting or failed merge stays live; successful zero-callback pairs
	// are safe to reuse regardless of the next caller's resolver identity.
	root, err := s.unionAt(left, right, observedResolve)
	if err == nil && !invoked {
		if s.unionCache == nil {
			s.unionCache = make(map[[2]string]string)
		}
		s.unionCache[key] = root
	}
	return root, err
}

// Filter retains entries accepted by keep. Untouched nodes retain their IDs.
func (s *ActionPlanInputSetStore) Filter(root string, keep func(ActionPlanInputSetEntry) (bool, error)) (string, error) {
	if keep == nil {
		return "", errors.New("input-set filter callback is nil")
	}
	if err := s.ready(); err != nil {
		return "", err
	}
	if root == "" {
		return "", nil
	}
	if err := s.validateRootReference(root); err != nil {
		return "", err
	}
	return s.filterAt(root, keep)
}

// Map transforms every entry. Target-stable transformations retain all
// unaffected subtrees; a transformation that moves any target is rebuilt once
// from the complete transformed set to preserve canonical radix placement.
func (s *ActionPlanInputSetStore) Map(root string, transform func(ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error)) (string, error) {
	if transform == nil {
		return "", errors.New("input-set map callback is nil")
	}
	if err := s.ready(); err != nil {
		return "", err
	}
	if root == "" {
		return "", nil
	}
	if err := s.validateRootReference(root); err != nil {
		return "", err
	}

	transformed := make(map[string]ActionPlanInputSetEntry)
	entries := make([]ActionPlanInputSetEntry, 0, s.nodes[root].Count)
	changed := false
	targetMoved := false
	err := s.walkTrie(root, func(entry ActionPlanInputSetEntry) error {
		mapped, err := transform(entry)
		if err != nil {
			return err
		}
		if err := s.validateEntry(mapped); err != nil {
			return fmt.Errorf("map input-set target %s: %w", actionPlanInputSetTargetDescription(entry.Target), err)
		}
		key := actionPlanInputSetTargetKey(mapped.Target)
		if previous, exists := transformed[key]; exists {
			return fmt.Errorf("map input set produced duplicate target %s from %s and %s", actionPlanInputSetTargetDescription(mapped.Target), actionPlanInputSetEntryProvenance(previous), actionPlanInputSetEntryProvenance(mapped))
		}
		transformed[key] = mapped
		entries = append(entries, mapped)
		changed = changed || mapped != entry
		targetMoved = targetMoved || mapped.Target != entry.Target
		return nil
	})
	if err != nil {
		return "", err
	}
	if !changed {
		return root, nil
	}
	if targetMoved {
		return s.build(0, entries)
	}
	return s.mapStableAt(root, transformed)
}

// NewTargetStableMapper creates a mapper which rejects target movement and
// memoizes immutable trie nodes across Map calls. The transform must be pure:
// memoized subtries do not replay callback side effects.
func (s *ActionPlanInputSetStore) NewTargetStableMapper(
	transform func(ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error),
) (*ActionPlanInputSetTargetStableMapper, error) {
	return s.newSelectiveTargetStableMapper(transform, nil)
}

// newSelectiveTargetStableMapper creates a mapper which descends only into
// subtries selected by mapSubtree. The selector must be stable for the
// mapper's lifetime and must select every ancestor of every entry which the
// transform could change. Unselected subtries retain their original IDs.
func (s *ActionPlanInputSetStore) newSelectiveTargetStableMapper(
	transform func(ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error),
	mapSubtree func(string) (bool, error),
) (*ActionPlanInputSetTargetStableMapper, error) {
	if transform == nil {
		return nil, errors.New("input-set target-stable map callback is nil")
	}
	if err := s.ready(); err != nil {
		return nil, err
	}
	return &ActionPlanInputSetTargetStableMapper{
		store:      s,
		transform:  transform,
		mapSubtree: mapSubtree,
		mapped:     make(map[string]string),
	}, nil
}

// Map transforms root with this mapper. Successful immutable subtries remain
// memoized for later roots; a failed node itself is never cached.
func (m *ActionPlanInputSetTargetStableMapper) Map(root string) (string, error) {
	if m == nil || m.store == nil || m.transform == nil || m.mapped == nil {
		return "", errors.New("input-set target-stable mapper is not initialized")
	}
	if root == "" {
		return "", nil
	}
	if err := m.store.validateRootReference(root); err != nil {
		return "", err
	}
	return m.mapAt(root)
}

func (m *ActionPlanInputSetTargetStableMapper) mapAt(id string) (string, error) {
	if mapped, ok := m.mapped[id]; ok {
		return mapped, nil
	}
	if m.mapSubtree != nil {
		selected, err := m.mapSubtree(id)
		if err != nil {
			return "", err
		}
		if !selected {
			m.mapped[id] = id
			return id, nil
		}
	}
	node, err := m.store.nodeAt(id)
	if err != nil {
		return "", err
	}
	if len(node.Entries) != 0 {
		entries := make([]ActionPlanInputSetEntry, len(node.Entries))
		changed := false
		for index, entry := range node.Entries {
			mapped, err := m.transform(entry)
			if err != nil {
				return "", err
			}
			if mapped.Target != entry.Target {
				return "", fmt.Errorf(
					"target-stable input-set map moved target %s to %s",
					actionPlanInputSetTargetDescription(entry.Target),
					actionPlanInputSetTargetDescription(mapped.Target),
				)
			}
			if err := m.store.validateEntry(mapped); err != nil {
				return "", fmt.Errorf("map input-set target %s: %w", actionPlanInputSetTargetDescription(entry.Target), err)
			}
			entries[index] = mapped
			changed = changed || mapped != entry
		}
		mappedID := id
		if changed {
			mappedID, err = m.store.build(node.Depth, entries)
			if err != nil {
				return "", err
			}
		}
		m.mapped[id] = mappedID
		return mappedID, nil
	}

	children := append([]ActionPlanInputSetChild(nil), node.Children...)
	changed := false
	for index := range children {
		mappedID, err := m.mapAt(children[index].ID)
		if err != nil {
			return "", err
		}
		changed = changed || mappedID != children[index].ID
		children[index].ID = mappedID
	}
	mappedID := id
	if changed {
		mappedID, err = m.store.internBranch(node.Depth, children)
		if err != nil {
			return "", err
		}
	}
	m.mapped[id] = mappedID
	return mappedID, nil
}

// Walk visits entries in lexical target order.
func (s *ActionPlanInputSetStore) Walk(root string, visit func(ActionPlanInputSetEntry) error) error {
	if visit == nil {
		return errors.New("input-set walk callback is nil")
	}
	if err := s.ready(); err != nil {
		return err
	}
	if root == "" {
		return nil
	}
	if err := s.validateRootReference(root); err != nil {
		return err
	}
	entries, err := s.collect(root)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return actionPlanInputSetTargetLess(entries[i].Target, entries[j].Target)
	})
	for _, entry := range entries {
		if err := visit(entry); err != nil {
			return err
		}
	}
	return nil
}

// ValidateRoot verifies IDs, canonical normalization, child references,
// counts, target placement, and all reachable entry invariants.
func (s *ActionPlanInputSetStore) ValidateRoot(root string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if root == "" {
		return nil
	}
	if err := s.validateRootReference(root); err != nil {
		return err
	}
	return s.validateReachable(root, "", make(map[string]bool), make(map[string]string))
}

func (s *ActionPlanInputSetStore) ready() error {
	if s == nil {
		return errors.New("input-set store is nil")
	}
	if s.nodes == nil || s.witnesses == nil || s.digest == nil || s.keyDigest == nil {
		return errors.New("input-set store is not initialized")
	}
	return nil
}

func (s *ActionPlanInputSetStore) validateRootReference(root string) error {
	if err := validatePlanDigest("input-set root", root); err != nil {
		return err
	}
	node, ok := s.nodes[root]
	if !ok {
		return fmt.Errorf("input-set root %q is missing", root)
	}
	if node.Depth != 0 {
		return fmt.Errorf("input-set root %q has depth %d, want 0", root, node.Depth)
	}
	return nil
}

func (s *ActionPlanInputSetStore) insertAt(id string, entry ActionPlanInputSetEntry) (string, error) {
	node, err := s.nodeAt(id)
	if err != nil {
		return "", err
	}
	if len(node.Entries) != 0 {
		entries := append([]ActionPlanInputSetEntry(nil), node.Entries...)
		key := actionPlanInputSetTargetKey(entry.Target)
		index := sort.Search(len(entries), func(index int) bool {
			return actionPlanInputSetTargetKey(entries[index].Target) >= key
		})
		if index < len(entries) && actionPlanInputSetTargetKey(entries[index].Target) == key {
			if entries[index] == entry {
				return id, nil
			}
			entries[index] = entry
		} else {
			entries = append(entries, ActionPlanInputSetEntry{})
			copy(entries[index+1:], entries[index:])
			entries[index] = entry
		}
		return s.build(node.Depth, entries)
	}

	nibble := s.targetNibble(entry.Target, node.Depth)
	index, found := actionPlanInputSetChildIndex(node.Children, nibble)
	children := append([]ActionPlanInputSetChild(nil), node.Children...)
	if found {
		childID, err := s.insertAt(children[index].ID, entry)
		if err != nil {
			return "", err
		}
		if childID == children[index].ID {
			return id, nil
		}
		children[index].ID = childID
	} else {
		childID, err := s.build(node.Depth+1, []ActionPlanInputSetEntry{entry})
		if err != nil {
			return "", err
		}
		children = append(children, ActionPlanInputSetChild{})
		copy(children[index+1:], children[index:])
		children[index] = ActionPlanInputSetChild{Nibble: nibble, ID: childID}
	}
	return s.internBranch(node.Depth, children)
}

func (s *ActionPlanInputSetStore) deleteAt(id string, target ActionPlanInputSetTarget) (string, bool, error) {
	node, err := s.nodeAt(id)
	if err != nil {
		return "", false, err
	}
	if len(node.Entries) != 0 {
		key := actionPlanInputSetTargetKey(target)
		index := sort.Search(len(node.Entries), func(index int) bool {
			return actionPlanInputSetTargetKey(node.Entries[index].Target) >= key
		})
		if index == len(node.Entries) || actionPlanInputSetTargetKey(node.Entries[index].Target) != key {
			return id, false, nil
		}
		entries := make([]ActionPlanInputSetEntry, 0, len(node.Entries)-1)
		entries = append(entries, node.Entries[:index]...)
		entries = append(entries, node.Entries[index+1:]...)
		if len(entries) == 0 {
			return "", true, nil
		}
		newID, err := s.build(node.Depth, entries)
		return newID, true, err
	}

	nibble := s.targetNibble(target, node.Depth)
	index, found := actionPlanInputSetChildIndex(node.Children, nibble)
	if !found {
		return id, false, nil
	}
	childID, removed, err := s.deleteAt(node.Children[index].ID, target)
	if err != nil || !removed {
		return id, removed, err
	}
	children := append([]ActionPlanInputSetChild(nil), node.Children...)
	if childID == "" {
		children = append(children[:index], children[index+1:]...)
	} else {
		children[index].ID = childID
	}
	if len(children) == 0 {
		return "", true, nil
	}
	count, err := s.childCount(children)
	if err != nil {
		return "", false, err
	}
	if count <= actionPlanInputSetLeafCapacity {
		entries, err := s.collectChildren(children, count)
		if err != nil {
			return "", false, err
		}
		newID, err := s.build(node.Depth, entries)
		return newID, true, err
	}
	newID, err := s.internBranch(node.Depth, children)
	return newID, true, err
}

func (s *ActionPlanInputSetStore) lookupAt(id string, target ActionPlanInputSetTarget) (ActionPlanInputSetEntry, bool, error) {
	node, err := s.nodeAt(id)
	if err != nil {
		return ActionPlanInputSetEntry{}, false, err
	}
	if len(node.Entries) != 0 {
		key := actionPlanInputSetTargetKey(target)
		index := sort.Search(len(node.Entries), func(index int) bool {
			return actionPlanInputSetTargetKey(node.Entries[index].Target) >= key
		})
		if index < len(node.Entries) && actionPlanInputSetTargetKey(node.Entries[index].Target) == key {
			return node.Entries[index], true, nil
		}
		return ActionPlanInputSetEntry{}, false, nil
	}
	nibble := s.targetNibble(target, node.Depth)
	index, found := actionPlanInputSetChildIndex(node.Children, nibble)
	if !found {
		return ActionPlanInputSetEntry{}, false, nil
	}
	return s.lookupAt(node.Children[index].ID, target)
}

func (s *ActionPlanInputSetStore) unionAt(leftID, rightID string, resolve ActionPlanInputSetConflictResolver) (string, error) {
	if leftID == rightID {
		return leftID, nil
	}
	left, err := s.nodeAt(leftID)
	if err != nil {
		return "", err
	}
	right, err := s.nodeAt(rightID)
	if err != nil {
		return "", err
	}
	if left.Depth != right.Depth {
		return "", fmt.Errorf("cannot union input-set nodes at depths %d and %d", left.Depth, right.Depth)
	}

	if len(left.Entries) != 0 && len(right.Entries) != 0 {
		entries, err := s.actionPlanInputSetMergeLeaves(left.Entries, right.Entries, resolve)
		if err != nil {
			return "", err
		}
		return s.build(left.Depth, entries)
	}
	if len(left.Children) != 0 && len(right.Children) != 0 {
		children := make([]ActionPlanInputSetChild, 0, len(left.Children)+len(right.Children))
		for leftIndex, rightIndex := 0, 0; leftIndex < len(left.Children) || rightIndex < len(right.Children); {
			switch {
			case rightIndex == len(right.Children) || leftIndex < len(left.Children) && left.Children[leftIndex].Nibble < right.Children[rightIndex].Nibble:
				children = append(children, left.Children[leftIndex])
				leftIndex++
			case leftIndex == len(left.Children) || right.Children[rightIndex].Nibble < left.Children[leftIndex].Nibble:
				children = append(children, right.Children[rightIndex])
				rightIndex++
			default:
				childID, err := s.unionAt(left.Children[leftIndex].ID, right.Children[rightIndex].ID, resolve)
				if err != nil {
					return "", err
				}
				children = append(children, ActionPlanInputSetChild{Nibble: left.Children[leftIndex].Nibble, ID: childID})
				leftIndex++
				rightIndex++
			}
		}
		return s.internBranch(left.Depth, children)
	}

	result := leftID
	incoming := right.Entries
	incomingIsRight := true
	if len(left.Entries) != 0 {
		result = rightID
		incoming = left.Entries
		incomingIsRight = false
	}
	for _, entry := range incoming {
		result, err = s.insertUnionAt(result, entry, incomingIsRight, resolve)
		if err != nil {
			return "", err
		}
	}
	return result, nil
}

func (s *ActionPlanInputSetStore) insertUnionAt(root string, incoming ActionPlanInputSetEntry, incomingIsRight bool, resolve ActionPlanInputSetConflictResolver) (string, error) {
	existing, found, err := s.lookupAt(root, incoming.Target)
	if err != nil {
		return "", err
	}
	if !found {
		return s.insertAt(root, incoming)
	}
	left, right := existing, incoming
	if !incomingIsRight {
		left, right = incoming, existing
	}
	merged, err := s.actionPlanInputSetResolveConflict(left, right, resolve)
	if err != nil {
		return "", err
	}
	return s.insertAt(root, merged)
}

func (s *ActionPlanInputSetStore) actionPlanInputSetMergeLeaves(left, right []ActionPlanInputSetEntry, resolve ActionPlanInputSetConflictResolver) ([]ActionPlanInputSetEntry, error) {
	result := make([]ActionPlanInputSetEntry, 0, len(left)+len(right))
	for leftIndex, rightIndex := 0, 0; leftIndex < len(left) || rightIndex < len(right); {
		switch {
		case rightIndex == len(right) || leftIndex < len(left) && actionPlanInputSetTargetKey(left[leftIndex].Target) < actionPlanInputSetTargetKey(right[rightIndex].Target):
			result = append(result, left[leftIndex])
			leftIndex++
		case leftIndex == len(left) || actionPlanInputSetTargetKey(right[rightIndex].Target) < actionPlanInputSetTargetKey(left[leftIndex].Target):
			result = append(result, right[rightIndex])
			rightIndex++
		default:
			entry, err := s.actionPlanInputSetResolveConflict(left[leftIndex], right[rightIndex], resolve)
			if err != nil {
				return nil, err
			}
			result = append(result, entry)
			leftIndex++
			rightIndex++
		}
	}
	return result, nil
}

func (s *ActionPlanInputSetStore) actionPlanInputSetResolveConflict(left, right ActionPlanInputSetEntry, resolve ActionPlanInputSetConflictResolver) (ActionPlanInputSetEntry, error) {
	if left == right {
		return left, nil
	}
	if resolve == nil {
		return ActionPlanInputSetEntry{}, fmt.Errorf("conflicting input-set entries at %s: %s versus %s", actionPlanInputSetTargetDescription(left.Target), actionPlanInputSetEntryProvenance(left), actionPlanInputSetEntryProvenance(right))
	}
	entry, err := resolve(left.Target, left, right)
	if err != nil {
		return ActionPlanInputSetEntry{}, err
	}
	if entry.Target != left.Target {
		return ActionPlanInputSetEntry{}, fmt.Errorf("input-set conflict resolver moved target %s to %s", actionPlanInputSetTargetDescription(left.Target), actionPlanInputSetTargetDescription(entry.Target))
	}
	if err := s.validateEntry(entry); err != nil {
		return ActionPlanInputSetEntry{}, fmt.Errorf("resolve input-set target %s: %w", actionPlanInputSetTargetDescription(left.Target), err)
	}
	return entry, nil
}

func (s *ActionPlanInputSetStore) filterAt(id string, keep func(ActionPlanInputSetEntry) (bool, error)) (string, error) {
	node, err := s.nodeAt(id)
	if err != nil {
		return "", err
	}
	if len(node.Entries) != 0 {
		entries := make([]ActionPlanInputSetEntry, 0, len(node.Entries))
		for _, entry := range node.Entries {
			accepted, err := keep(entry)
			if err != nil {
				return "", err
			}
			if accepted {
				entries = append(entries, entry)
			}
		}
		if len(entries) == len(node.Entries) {
			return id, nil
		}
		if len(entries) == 0 {
			return "", nil
		}
		return s.build(node.Depth, entries)
	}

	children := make([]ActionPlanInputSetChild, 0, len(node.Children))
	changed := false
	for _, child := range node.Children {
		childID, err := s.filterAt(child.ID, keep)
		if err != nil {
			return "", err
		}
		changed = changed || childID != child.ID
		if childID != "" {
			children = append(children, ActionPlanInputSetChild{Nibble: child.Nibble, ID: childID})
		}
	}
	if !changed {
		return id, nil
	}
	if len(children) == 0 {
		return "", nil
	}
	count, err := s.childCount(children)
	if err != nil {
		return "", err
	}
	if count <= actionPlanInputSetLeafCapacity {
		entries, err := s.collectChildren(children, count)
		if err != nil {
			return "", err
		}
		return s.build(node.Depth, entries)
	}
	return s.internBranch(node.Depth, children)
}

func (s *ActionPlanInputSetStore) mapStableAt(id string, transformed map[string]ActionPlanInputSetEntry) (string, error) {
	node, err := s.nodeAt(id)
	if err != nil {
		return "", err
	}
	if len(node.Entries) != 0 {
		entries := make([]ActionPlanInputSetEntry, len(node.Entries))
		changed := false
		for index, entry := range node.Entries {
			entries[index] = transformed[actionPlanInputSetTargetKey(entry.Target)]
			changed = changed || entries[index] != entry
		}
		if !changed {
			return id, nil
		}
		return s.build(node.Depth, entries)
	}
	children := append([]ActionPlanInputSetChild(nil), node.Children...)
	changed := false
	for index := range children {
		childID, err := s.mapStableAt(children[index].ID, transformed)
		if err != nil {
			return "", err
		}
		changed = changed || childID != children[index].ID
		children[index].ID = childID
	}
	if !changed {
		return id, nil
	}
	return s.internBranch(node.Depth, children)
}

func (s *ActionPlanInputSetStore) build(depth int, entries []ActionPlanInputSetEntry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	if depth < 0 || depth > actionPlanInputSetDigestNibbles {
		return "", fmt.Errorf("input-set node depth %d is outside [0,%d]", depth, actionPlanInputSetDigestNibbles)
	}
	entries = append([]ActionPlanInputSetEntry(nil), entries...)
	for _, entry := range entries {
		if err := s.validateEntry(entry); err != nil {
			return "", err
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return actionPlanInputSetTargetKey(entries[i].Target) < actionPlanInputSetTargetKey(entries[j].Target)
	})
	for index := 1; index < len(entries); index++ {
		if entries[index-1].Target == entries[index].Target {
			return "", fmt.Errorf("duplicate input-set target %s", actionPlanInputSetTargetDescription(entries[index].Target))
		}
	}
	if len(entries) <= actionPlanInputSetLeafCapacity {
		return s.intern(ActionPlanInputSetNode{
			Schema:  ActionPlanInputSetSchemaVersion,
			Kind:    ActionPlanInputSetLeafNode,
			Depth:   depth,
			Count:   len(entries),
			Entries: entries,
		})
	}
	if depth == actionPlanInputSetDigestNibbles {
		return "", fmt.Errorf("more than %d input-set targets have the same SHA-256 target digest", actionPlanInputSetLeafCapacity)
	}

	buckets := make(map[string][]ActionPlanInputSetEntry, 16)
	for _, entry := range entries {
		nibble := s.targetNibble(entry.Target, depth)
		buckets[nibble] = append(buckets[nibble], entry)
	}
	nibbles := make([]string, 0, len(buckets))
	for nibble := range buckets {
		nibbles = append(nibbles, nibble)
	}
	sort.Strings(nibbles)
	children := make([]ActionPlanInputSetChild, 0, len(nibbles))
	for _, nibble := range nibbles {
		childID, err := s.build(depth+1, buckets[nibble])
		if err != nil {
			return "", err
		}
		children = append(children, ActionPlanInputSetChild{Nibble: nibble, ID: childID})
	}
	return s.internBranch(depth, children)
}

func (s *ActionPlanInputSetStore) internBranch(depth int, children []ActionPlanInputSetChild) (string, error) {
	count, err := s.childCount(children)
	if err != nil {
		return "", err
	}
	if count <= actionPlanInputSetLeafCapacity {
		return "", fmt.Errorf("input-set branch at depth %d contains %d entries; canonical branches require more than %d", depth, count, actionPlanInputSetLeafCapacity)
	}
	return s.intern(ActionPlanInputSetNode{
		Schema:   ActionPlanInputSetSchemaVersion,
		Kind:     ActionPlanInputSetBranchNode,
		Depth:    depth,
		Count:    count,
		Children: append([]ActionPlanInputSetChild(nil), children...),
	})
}

func (s *ActionPlanInputSetStore) intern(node ActionPlanInputSetNode) (string, error) {
	canonical, err := actionPlanInputSetCanonicalNode(node)
	if err != nil {
		return "", err
	}
	id := s.digest(canonical)
	if witness, exists := s.witnesses[id]; exists {
		if string(witness) != string(canonical) {
			return "", fmt.Errorf("input-set content ID collision for %q", id)
		}
		return id, nil
	}
	if previous, exists := s.nodes[id]; exists {
		previousCanonical, err := actionPlanInputSetCanonicalNode(previous)
		if err != nil || string(previousCanonical) != string(canonical) {
			return "", fmt.Errorf("input-set content ID collision for %q", id)
		}
	}
	s.nodes[id] = cloneActionPlanInputSetNode(node)
	s.witnesses[id] = append([]byte(nil), canonical...)
	return id, nil
}

func (s *ActionPlanInputSetStore) nodeAt(id string) (ActionPlanInputSetNode, error) {
	node, ok := s.nodes[id]
	if !ok {
		return ActionPlanInputSetNode{}, fmt.Errorf("input-set node %q is missing", id)
	}
	return node, nil
}

func (s *ActionPlanInputSetStore) childCount(children []ActionPlanInputSetChild) (int, error) {
	count := 0
	for _, child := range children {
		node, err := s.nodeAt(child.ID)
		if err != nil {
			return 0, err
		}
		count += node.Count
	}
	return count, nil
}

func (s *ActionPlanInputSetStore) collect(root string) ([]ActionPlanInputSetEntry, error) {
	node, err := s.nodeAt(root)
	if err != nil {
		return nil, err
	}
	entries := make([]ActionPlanInputSetEntry, 0, node.Count)
	err = s.walkTrie(root, func(entry ActionPlanInputSetEntry) error {
		entries = append(entries, entry)
		return nil
	})
	return entries, err
}

func (s *ActionPlanInputSetStore) collectChildren(children []ActionPlanInputSetChild, capacity int) ([]ActionPlanInputSetEntry, error) {
	entries := make([]ActionPlanInputSetEntry, 0, capacity)
	for _, child := range children {
		if err := s.walkTrie(child.ID, func(entry ActionPlanInputSetEntry) error {
			entries = append(entries, entry)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func (s *ActionPlanInputSetStore) walkTrie(root string, visit func(ActionPlanInputSetEntry) error) error {
	node, err := s.nodeAt(root)
	if err != nil {
		return err
	}
	if len(node.Entries) != 0 {
		for _, entry := range node.Entries {
			if err := visit(entry); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range node.Children {
		if err := s.walkTrie(child.ID, visit); err != nil {
			return err
		}
	}
	return nil
}

func (s *ActionPlanInputSetStore) validateAllNodes() error {
	for id, node := range s.nodes {
		if err := s.validateNodeLocal(id, node); err != nil {
			return err
		}
	}
	validated := make(map[string]string, len(s.nodes))
	visiting := make(map[string]bool)
	for id, node := range s.nodes {
		if node.Depth == 0 {
			if err := s.validateReachable(id, "", visiting, validated); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *ActionPlanInputSetStore) validateNodeLocal(id string, node ActionPlanInputSetNode) error {
	if node.Schema != ActionPlanInputSetSchemaVersion {
		return fmt.Errorf("input-set node %q has schema %q, want %q", id, node.Schema, ActionPlanInputSetSchemaVersion)
	}
	if node.Depth < 0 || node.Depth > actionPlanInputSetDigestNibbles {
		return fmt.Errorf("input-set node %q has invalid depth %d", id, node.Depth)
	}
	canonical, err := actionPlanInputSetCanonicalNode(node)
	if err != nil {
		return fmt.Errorf("encode input-set node %q: %w", id, err)
	}
	if got := s.digest(canonical); got != id {
		return fmt.Errorf("input-set node %q has canonical ID %q", id, got)
	}
	witness, ok := s.witnesses[id]
	if !ok || string(witness) != string(canonical) {
		return fmt.Errorf("input-set node %q has no matching collision witness", id)
	}
	leaf := len(node.Entries) != 0
	branch := len(node.Children) != 0
	if leaf == branch {
		return fmt.Errorf("input-set node %q must be exactly one of leaf or branch", id)
	}
	if leaf {
		if node.Kind != ActionPlanInputSetLeafNode {
			return fmt.Errorf("input-set leaf %q has kind %q", id, node.Kind)
		}
		if len(node.Entries) > actionPlanInputSetLeafCapacity {
			return fmt.Errorf("input-set leaf %q contains %d entries, maximum is %d", id, len(node.Entries), actionPlanInputSetLeafCapacity)
		}
		if node.Count != len(node.Entries) {
			return fmt.Errorf("input-set leaf %q count is %d, want %d", id, node.Count, len(node.Entries))
		}
		previous := ""
		for index, entry := range node.Entries {
			if err := s.validateEntry(entry); err != nil {
				return fmt.Errorf("input-set leaf %q entry %d: %w", id, index, err)
			}
			key := actionPlanInputSetTargetKey(entry.Target)
			if index > 0 && previous >= key {
				return fmt.Errorf("input-set leaf %q entries are not in strict target order", id)
			}
			previous = key
		}
		return nil
	}

	if node.Kind != ActionPlanInputSetBranchNode {
		return fmt.Errorf("input-set branch %q has kind %q", id, node.Kind)
	}
	if node.Depth == actionPlanInputSetDigestNibbles {
		return fmt.Errorf("input-set branch %q is below the target digest", id)
	}
	if node.Count <= actionPlanInputSetLeafCapacity {
		return fmt.Errorf("input-set branch %q contains %d entries and is not canonically collapsed", id, node.Count)
	}
	if len(node.Children) > 16 {
		return fmt.Errorf("input-set branch %q has %d children", id, len(node.Children))
	}
	count := 0
	previous := ""
	for index, child := range node.Children {
		if !actionPlanInputSetValidNibble(child.Nibble) {
			return fmt.Errorf("input-set branch %q child %d has invalid nibble %q", id, index, child.Nibble)
		}
		if index > 0 && previous >= child.Nibble {
			return fmt.Errorf("input-set branch %q children are not in strict nibble order", id)
		}
		if err := validatePlanDigest("input-set child ID", child.ID); err != nil {
			return fmt.Errorf("input-set branch %q: %w", id, err)
		}
		childNode, ok := s.nodes[child.ID]
		if !ok {
			return fmt.Errorf("input-set branch %q references missing child %q", id, child.ID)
		}
		if childNode.Depth != node.Depth+1 {
			return fmt.Errorf("input-set branch %q child %q has depth %d, want %d", id, child.ID, childNode.Depth, node.Depth+1)
		}
		count += childNode.Count
		previous = child.Nibble
	}
	if count != node.Count {
		return fmt.Errorf("input-set branch %q count is %d, child sum is %d", id, node.Count, count)
	}
	return nil
}

func (s *ActionPlanInputSetStore) validateReachable(id, prefix string, visiting map[string]bool, validated map[string]string) error {
	if previous, ok := validated[id]; ok {
		if previous != prefix {
			return fmt.Errorf("input-set node %q is reachable below conflicting prefixes %q and %q", id, previous, prefix)
		}
		return nil
	}
	if visiting[id] {
		return fmt.Errorf("input-set node cycle at %q", id)
	}
	node, ok := s.nodes[id]
	if !ok {
		return fmt.Errorf("input-set node %q is missing", id)
	}
	if err := s.validateNodeLocal(id, node); err != nil {
		return err
	}
	if node.Depth != len(prefix) {
		return fmt.Errorf("input-set node %q depth is %d, traversal prefix has length %d", id, node.Depth, len(prefix))
	}
	visiting[id] = true
	defer delete(visiting, id)
	if len(node.Entries) != 0 {
		for _, entry := range node.Entries {
			digest := s.keyDigest(entry.Target)
			for depth := range prefix {
				want := string(prefix[depth])
				if got := actionPlanInputSetDigestNibble(digest, depth); got != want {
					return fmt.Errorf("input-set leaf %q places target %s below nibble %q at depth %d, want %q", id, actionPlanInputSetTargetDescription(entry.Target), got, depth, want)
				}
			}
		}
		validated[id] = prefix
		return nil
	}
	for _, child := range node.Children {
		if err := s.validateReachable(child.ID, prefix+child.Nibble, visiting, validated); err != nil {
			return err
		}
	}
	validated[id] = prefix
	return nil
}

func actionPlanInputSetCanonicalNode(node ActionPlanInputSetNode) ([]byte, error) {
	if len(node.Children) == 0 {
		node.Children = nil
	}
	if len(node.Entries) == 0 {
		node.Entries = nil
	}
	return json.Marshal(node)
}

func cloneActionPlanInputSetNode(node ActionPlanInputSetNode) ActionPlanInputSetNode {
	node.Children = append([]ActionPlanInputSetChild(nil), node.Children...)
	node.Entries = append([]ActionPlanInputSetEntry(nil), node.Entries...)
	return node
}

func validateActionPlanInputSetTarget(target ActionPlanInputSetTarget) error {
	switch target.Kind {
	case ActionPlanInputSetWorkTarget, ActionPlanInputSetAmbientTarget:
		if target.Tree != "" {
			return fmt.Errorf("input-set %s target has unexpected tree %q", target.Kind, target.Tree)
		}
	case ActionPlanInputSetTreeTarget:
		if err := validatePlanName("input-set target tree", target.Tree); err != nil {
			return err
		}
	default:
		return fmt.Errorf("input-set target has unknown kind %q", target.Kind)
	}
	return validatePlanRelativePath("input-set target", target.Path)
}

func (s *ActionPlanInputSetStore) validateEntry(entry ActionPlanInputSetEntry) error {
	allowProvisional := s != nil && s.allowProvisionalProducerIDs
	return validateActionPlanInputSetEntryMode(entry, allowProvisional)
}

func validateActionPlanInputSetEntry(entry ActionPlanInputSetEntry) error {
	return validateActionPlanInputSetEntryMode(entry, false)
}

func validateActionPlanInputSetEntryMode(entry ActionPlanInputSetEntry, allowProvisionalProducerID bool) error {
	if err := validateActionPlanInputSetTarget(entry.Target); err != nil {
		return err
	}
	if entry.AuxiliaryUse && !entry.CompilerUse {
		return fmt.Errorf("input-set target %s has auxiliary use without compiler use", actionPlanInputSetTargetDescription(entry.Target))
	}
	hasSource := entry.SourceID != ""
	hasProducer := entry.ProducerID != ""
	if hasSource == hasProducer {
		return fmt.Errorf("input-set target %s must have exactly one of source or producer provenance", actionPlanInputSetTargetDescription(entry.Target))
	}
	if hasSource {
		if !validSourceID(entry.SourceID) && !validFamilySourceID(entry.SourceID) {
			return fmt.Errorf("input-set source ID %q is not canonical", entry.SourceID)
		}
		if entry.Slot != 0 {
			return fmt.Errorf("input-set source %q has producer slot %d", entry.SourceID, entry.Slot)
		}
		return nil
	}
	if err := validatePlanDigest("input-set producer ID", entry.ProducerID); err != nil {
		if !allowProvisionalProducerID {
			return err
		}
		if provisionalErr := validatePlanName("provisional input-set producer ID", entry.ProducerID); provisionalErr != nil {
			return provisionalErr
		}
	}
	if entry.Slot < 0 || entry.Slot > maximumActionPlanOrdinal {
		return fmt.Errorf("input-set producer slot %d is outside [0,%d]", entry.Slot, maximumActionPlanOrdinal)
	}
	return nil
}

// actionPlanInputSetTargetLess compares already-validated target tuples in
// their canonical NUL-delimited key order without allocating a key for each
// comparison. Target kinds and tree names contain no NUL bytes, so a field's
// terminator sorts before every byte of a longer valid field. Path is last.
func actionPlanInputSetTargetLess(left, right ActionPlanInputSetTarget) bool {
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.Tree != right.Tree {
		return left.Tree < right.Tree
	}
	return left.Path < right.Path
}

func actionPlanInputSetTargetKey(target ActionPlanInputSetTarget) string {
	return string(target.Kind) + "\x00" + target.Tree + "\x00" + target.Path
}

func actionPlanInputSetTargetDescription(target ActionPlanInputSetTarget) string {
	if target.Tree != "" {
		return fmt.Sprintf("%s:%s:%s", target.Kind, target.Tree, target.Path)
	}
	return fmt.Sprintf("%s:%s", target.Kind, target.Path)
}

func actionPlanInputSetEntryProvenance(entry ActionPlanInputSetEntry) string {
	if entry.SourceID != "" {
		return entry.SourceID
	}
	return fmt.Sprintf("%s[%d]", entry.ProducerID, entry.Slot)
}

func actionPlanInputSetTargetDigest(target ActionPlanInputSetTarget) [sha256.Size]byte {
	return sha256.Sum256([]byte(actionPlanInputSetTargetKey(target)))
}

func actionPlanInputSetTargetNibble(target ActionPlanInputSetTarget, depth int) string {
	return actionPlanInputSetDigestNibble(actionPlanInputSetTargetDigest(target), depth)
}

func (s *ActionPlanInputSetStore) targetNibble(target ActionPlanInputSetTarget, depth int) string {
	return actionPlanInputSetDigestNibble(s.keyDigest(target), depth)
}

func actionPlanInputSetDigestNibble(digest [sha256.Size]byte, depth int) string {
	value := digest[depth/2]
	if depth%2 == 0 {
		value >>= 4
	} else {
		value &= 0x0f
	}
	return string("0123456789abcdef"[value])
}

func actionPlanInputSetValidNibble(value string) bool {
	return len(value) == 1 && strings.Contains("0123456789abcdef", value)
}

func actionPlanInputSetChildIndex(children []ActionPlanInputSetChild, nibble string) (int, bool) {
	index := sort.Search(len(children), func(index int) bool { return children[index].Nibble >= nibble })
	return index, index < len(children) && children[index].Nibble == nibble
}
