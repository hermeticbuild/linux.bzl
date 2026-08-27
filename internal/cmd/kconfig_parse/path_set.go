package main

import "crypto/sha256"

// kbuildPathSet is an immutable ordered set of canonical path strings. Add
// path-copies only the treap search path, so prior snapshots remain unchanged
// and untouched subtrees stay shared. Node priorities depend only on paths;
// consequently, equal sets have the same tree shape regardless of insertion
// order.
//
// Callers own path normalization. The set orders and compares the supplied
// canonical spellings byte-for-byte.
type kbuildPathSet struct {
	root *kbuildPathSetNode
}

type kbuildPathSetNode struct {
	path     string
	left     *kbuildPathSetNode
	right    *kbuildPathSetNode
	size     int
	priority [sha256.Size]byte
}

func emptyKbuildPathSet() kbuildPathSet {
	return kbuildPathSet{}
}

// kbuildPathSetAdd returns a set containing path. Adding a path already in the
// set is allocation-free and returns the original root.
func kbuildPathSetAdd(set kbuildPathSet, path string) kbuildPathSet {
	return kbuildPathSet{root: kbuildPathSetAddNode(set.root, path)}
}

func kbuildPathSetContains(set kbuildPathSet, path string) bool {
	for node := set.root; node != nil; {
		switch {
		case path < node.path:
			node = node.left
		case path > node.path:
			node = node.right
		default:
			return true
		}
	}
	return false
}

func kbuildPathSetLen(set kbuildPathSet) int {
	return kbuildPathSetNodeSize(set.root)
}

// kbuildPathSetRange visits paths in ascending order. Iteration stops when
// visit returns false.
func kbuildPathSetRange(set kbuildPathSet, visit func(path string) bool) {
	var walk func(*kbuildPathSetNode) bool
	walk = func(node *kbuildPathSetNode) bool {
		if node == nil {
			return true
		}
		return walk(node.left) && visit(node.path) && walk(node.right)
	}
	walk(set.root)
}

// kbuildPathSetUnion returns the union of left and right. It starts with the
// larger immutable snapshot and inserts only the smaller set, minimizing tree
// searches and preserving the larger root exactly when it is already a
// superset.
func kbuildPathSetUnion(left, right kbuildPathSet) kbuildPathSet {
	if left.root == right.root {
		return left
	}
	if kbuildPathSetLen(left) < kbuildPathSetLen(right) {
		left, right = right, left
	}
	result := left
	kbuildPathSetRange(right, func(path string) bool {
		result = kbuildPathSetAdd(result, path)
		return true
	})
	return result
}

func kbuildPathSetAddNode(node *kbuildPathSetNode, path string) *kbuildPathSetNode {
	if node == nil {
		return newKbuildPathSetNode(path, nil, nil)
	}

	switch {
	case path < node.path:
		left := kbuildPathSetAddNode(node.left, path)
		if left == node.left {
			return node
		}
		updated := newKbuildPathSetNode(node.path, left, node.right)
		if kbuildPathSetPriorityLess(updated.left, updated) {
			return kbuildPathSetRotateRight(updated)
		}
		return updated
	case path > node.path:
		right := kbuildPathSetAddNode(node.right, path)
		if right == node.right {
			return node
		}
		updated := newKbuildPathSetNode(node.path, node.left, right)
		if kbuildPathSetPriorityLess(updated.right, updated) {
			return kbuildPathSetRotateLeft(updated)
		}
		return updated
	default:
		return node
	}
}

func newKbuildPathSetNode(path string, left, right *kbuildPathSetNode) *kbuildPathSetNode {
	return &kbuildPathSetNode{
		path:     path,
		left:     left,
		right:    right,
		size:     kbuildPathSetNodeSize(left) + kbuildPathSetNodeSize(right) + 1,
		priority: sha256.Sum256([]byte("linux-bzl-kbuild-path-set-priority-v1\x00" + path)),
	}
}

func kbuildPathSetPriorityLess(left, right *kbuildPathSetNode) bool {
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

func kbuildPathSetRotateLeft(node *kbuildPathSetNode) *kbuildPathSetNode {
	right := node.right
	left := newKbuildPathSetNode(node.path, node.left, right.left)
	return newKbuildPathSetNode(right.path, left, right.right)
}

func kbuildPathSetRotateRight(node *kbuildPathSetNode) *kbuildPathSetNode {
	left := node.left
	right := newKbuildPathSetNode(node.path, left.right, node.right)
	return newKbuildPathSetNode(left.path, left.left, right)
}

func kbuildPathSetNodeSize(node *kbuildPathSetNode) int {
	if node == nil {
		return 0
	}
	return node.size
}
