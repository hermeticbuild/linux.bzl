package main

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// kbuildFrontierValue is the version of one object-tree path at a causal
// Kbuild frontier. A missing path is represented by its absence from the
// kbuildFrontierState rather than by a sentinel value.
type kbuildFrontierValue struct {
	artifact kconfig.CompactKbuildVisibleArtifact
	content  string
	exact    bool
	origin   *kbuildRecursiveMakeFrontier
}

// kbuildFrontierEntry is one sorted input to
// newKbuildFrontierStateFromSorted. Inputs must have strictly increasing,
// unique paths.
type kbuildFrontierEntry struct {
	path  string
	value kbuildFrontierValue
}

// kbuildFrontierState is an immutable ordered map. Updating a state path-copies
// only the canonical treap search path, so older causal-frontier snapshots
// remain valid and unchanged while untouched subtrees are shared. Priorities
// are derived from paths, making the tree shape independent of insertion order.
type kbuildFrontierState struct {
	root *kbuildFrontierNode
}

type kbuildFrontierNode struct {
	path   string
	value  kbuildFrontierValue
	left   *kbuildFrontierNode
	right  *kbuildFrontierNode
	height int
	size   int
	// priority is derived solely from path. The persistent tree is therefore a
	// canonical treap: insertion history cannot change its shape or digest for
	// the same resolved visible-file map.
	priority    [sha256.Size]byte
	valueDigest [sha256.Size]byte
	digest      [sha256.Size]byte
}

// newKbuildFrontierStateFromSorted constructs a canonical state.
// entries must be ordered by strictly increasing path and contain no duplicate
// paths.
func newKbuildFrontierStateFromSorted(entries []kbuildFrontierEntry) kbuildFrontierState {
	state := kbuildFrontierState{}
	for _, entry := range entries {
		state = kbuildFrontierSet(state, entry.path, entry.value)
	}
	return state
}

func kbuildFrontierGet(state kbuildFrontierState, path string) (kbuildFrontierValue, bool) {
	for node := state.root; node != nil; {
		switch {
		case path < node.path:
			node = node.left
		case path > node.path:
			node = node.right
		default:
			return node.value, true
		}
	}
	return kbuildFrontierValue{}, false
}

// kbuildFrontierSet returns a new state containing value at path. The input
// state and every node reachable only through it remain unchanged.
func kbuildFrontierSet(state kbuildFrontierState, path string, value kbuildFrontierValue) kbuildFrontierState {
	return kbuildFrontierState{root: kbuildFrontierSetNode(state.root, path, value)}
}

// kbuildFrontierRange visits entries in ascending path order. Iteration stops
// when visit returns false.
func kbuildFrontierRange(state kbuildFrontierState, visit func(string, kbuildFrontierValue) bool) {
	var walk func(*kbuildFrontierNode) bool
	walk = func(node *kbuildFrontierNode) bool {
		if node == nil {
			return true
		}
		return walk(node.left) && visit(node.path, node.value) && walk(node.right)
	}
	walk(state.root)
}

// kbuildFrontierRangePrefix visits only entries whose paths start with prefix,
// in ascending order. The tree search skips subtrees outside the prefix range;
// an empty prefix is equivalent to kbuildFrontierRange.
func kbuildFrontierRangePrefix(
	state kbuildFrontierState,
	prefix string,
	visit func(string, kbuildFrontierValue) bool,
) {
	if prefix == "" {
		kbuildFrontierRange(state, visit)
		return
	}
	var walk func(*kbuildFrontierNode) bool
	walk = func(node *kbuildFrontierNode) bool {
		if node == nil {
			return true
		}
		if node.path >= prefix && !walk(node.left) {
			return false
		}
		matches := strings.HasPrefix(node.path, prefix)
		if matches && !visit(node.path, node.value) {
			return false
		}
		if node.path < prefix || matches {
			return walk(node.right)
		}
		return true
	}
	walk(state.root)
}

func kbuildFrontierLen(state kbuildFrontierState) int {
	return kbuildFrontierNodeSize(state.root)
}

// kbuildFrontierDigest is an O(1) identity for one immutable frontier root.
// It includes the exact visible owner/content map but deliberately excludes
// causal origin pointers, matching request identity's observable filesystem
// semantics. The canonical treap shape is part of the identity, so equivalent
// maps have equal digests regardless of insertion history and can be compared
// without flattening their entries.
func kbuildFrontierDigest(state kbuildFrontierState) [sha256.Size]byte {
	if state.root == nil {
		return sha256.Sum256([]byte("linux-bzl-kbuild-frontier-empty-v1"))
	}
	return state.root.digest
}

func kbuildFrontierSetNode(node *kbuildFrontierNode, path string, value kbuildFrontierValue) *kbuildFrontierNode {
	if node == nil {
		return newKbuildFrontierNode(path, value, nil, nil)
	}

	switch {
	case path < node.path:
		updated := rebuildKbuildFrontierNode(
			node,
			kbuildFrontierSetNode(node.left, path, value),
			node.right,
		)
		if kbuildFrontierPriorityLess(updated.left, updated) {
			return kbuildFrontierRotateRight(updated)
		}
		return updated
	case path > node.path:
		updated := rebuildKbuildFrontierNode(
			node,
			node.left,
			kbuildFrontierSetNode(node.right, path, value),
		)
		if kbuildFrontierPriorityLess(updated.right, updated) {
			return kbuildFrontierRotateLeft(updated)
		}
		return updated
	default:
		return newKbuildFrontierNode(path, value, node.left, node.right)
	}
}

func kbuildFrontierPriorityLess(left, right *kbuildFrontierNode) bool {
	if left == nil {
		return false
	}
	for index := range left.priority {
		if left.priority[index] != right.priority[index] {
			return left.priority[index] < right.priority[index]
		}
	}
	return left.path < right.path
}

func kbuildFrontierRotateLeft(node *kbuildFrontierNode) *kbuildFrontierNode {
	right := node.right
	left := rebuildKbuildFrontierNode(node, node.left, right.left)
	return rebuildKbuildFrontierNode(right, left, right.right)
}

func kbuildFrontierRotateRight(node *kbuildFrontierNode) *kbuildFrontierNode {
	left := node.left
	right := rebuildKbuildFrontierNode(node, left.right, node.right)
	return rebuildKbuildFrontierNode(left, left.left, right)
}

func newKbuildFrontierNode(
	path string,
	value kbuildFrontierValue,
	left, right *kbuildFrontierNode,
) *kbuildFrontierNode {
	node := &kbuildFrontierNode{
		path:        path,
		value:       value,
		left:        left,
		right:       right,
		priority:    sha256.Sum256([]byte("linux-bzl-kbuild-frontier-priority-v1\x00" + path)),
		valueDigest: kbuildFrontierValueDigest(path, value),
	}
	kbuildFrontierFinishNode(node)
	return node
}

// rebuildKbuildFrontierNode changes only child links. Observable bytes and
// path priority are retained from source, so path-copying an update never
// re-hashes an unrelated ancestor's potentially large generated contents.
func rebuildKbuildFrontierNode(
	source *kbuildFrontierNode,
	left, right *kbuildFrontierNode,
) *kbuildFrontierNode {
	node := &kbuildFrontierNode{
		path:        source.path,
		value:       source.value,
		left:        left,
		right:       right,
		priority:    source.priority,
		valueDigest: source.valueDigest,
	}
	kbuildFrontierFinishNode(node)
	return node
}

func kbuildFrontierValueDigest(path string, value kbuildFrontierValue) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte("linux-bzl-kbuild-frontier-value-v1"))
	writeDigestString := func(value string) {
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(value))
	}
	writeDigestString(path)
	writeDigestString(value.artifact.Path)
	writeDigestString(value.artifact.Profile)
	writeDigestString(value.artifact.Target)
	if value.exact {
		_, _ = hash.Write([]byte{1})
		writeDigestString(value.content)
	} else {
		_, _ = hash.Write([]byte{0})
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

func kbuildFrontierFinishNode(node *kbuildFrontierNode) {
	node.height = max(kbuildFrontierNodeHeight(node.left), kbuildFrontierNodeHeight(node.right)) + 1
	node.size = kbuildFrontierNodeSize(node.left) + kbuildFrontierNodeSize(node.right) + 1
	hash := sha256.New()
	_, _ = hash.Write([]byte("linux-bzl-kbuild-frontier-node-v2"))
	_, _ = hash.Write(node.valueDigest[:])
	for _, child := range []*kbuildFrontierNode{node.left, node.right} {
		if child == nil {
			_, _ = hash.Write(make([]byte, sha256.Size))
		} else {
			_, _ = hash.Write(child.digest[:])
		}
	}
	copy(node.digest[:], hash.Sum(nil))
}

func kbuildFrontierNodeHeight(node *kbuildFrontierNode) int {
	if node == nil {
		return 0
	}
	return node.height
}

func kbuildFrontierNodeSize(node *kbuildFrontierNode) int {
	if node == nil {
		return 0
	}
	return node.size
}
