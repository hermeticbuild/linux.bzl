package kconfig

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// configDependencyMacroSnapshotCell is the complete state observable by the
// conditional preprocessor model. recorded distinguishes an explicit
// Undefined/Unknown value from ordinary absence; generated-header guards rely
// on that distinction even when the three-valued definition is unchanged.
type configDependencyMacroSnapshotCell struct {
	definition configDependencyMacroDefinition
	recorded   bool
}

const configDependencyMacroSnapshotCellCount = 6

const configDependencyMacroSnapshotConservativeCellIndex uint8 = 1

func configDependencyMacroSnapshotCellIndex(
	cell configDependencyMacroSnapshotCell,
) (int, bool) {
	definitionIndex := int(cell.definition)
	if cell.definition == configDependencyMacroUnknown {
		definitionIndex = 0
	} else if cell.definition < configDependencyMacroUndefined || cell.definition > configDependencyMacroDefined {
		return 0, false
	}
	index := 2 * definitionIndex
	if cell.recorded {
		index++
	}
	return index, true
}

func configDependencyMacroSnapshotCellAt(index int) (
	configDependencyMacroSnapshotCell,
	bool,
) {
	if index < 0 || index >= configDependencyMacroSnapshotCellCount {
		return configDependencyMacroSnapshotCell{}, false
	}
	definition := configDependencyMacroDefinition(index / 2)
	if definition == 0 {
		definition = configDependencyMacroUnknown
	}
	return configDependencyMacroSnapshotCell{
		definition: definition,
		recorded:   index%2 != 0,
	}, true
}

// A transform is an exact pointwise function over the six macro cells. Each
// result is stored as its three-bit cell index instead of repeating the cell
// structs in every persistent-tree edge. Arrays are values, so tags embedded
// in tree views remain immutable and comparable. This also makes composition
// and branch joining independent of assumptions about which transforms happen
// to be reachable today.
type configDependencyMacroSnapshotTransform [configDependencyMacroSnapshotCellCount]uint8

func configDependencyMacroSnapshotPackCell(cell configDependencyMacroSnapshotCell) uint8 {
	index, valid := configDependencyMacroSnapshotCellIndex(cell)
	if valid {
		return uint8(index)
	}
	// Internal transform construction supplies only valid cells. Preserve the
	// existing conservative behavior if corrupt internal state reaches here.
	return configDependencyMacroSnapshotConservativeCellIndex
}

func configDependencyMacroSnapshotIdentityTransform() configDependencyMacroSnapshotTransform {
	return configDependencyMacroSnapshotTransform{0, 1, 2, 3, 4, 5}
}

func configDependencyMacroSnapshotMaybeDefineCell(
	cell configDependencyMacroSnapshotCell,
) configDependencyMacroSnapshotCell {
	definition := configDependencyMacroUnknown
	if cell.definition == configDependencyMacroDefined {
		definition = configDependencyMacroDefined
	}
	return configDependencyMacroSnapshotCell{definition: definition, recorded: true}
}

func configDependencyMacroSnapshotMaybeDefineTransform() configDependencyMacroSnapshotTransform {
	return configDependencyMacroSnapshotTransform{1, 1, 1, 1, 5, 5}
}

func configDependencyMacroSnapshotApplyTransform(
	transform configDependencyMacroSnapshotTransform,
	cell configDependencyMacroSnapshotCell,
) (configDependencyMacroSnapshotCell, bool) {
	index, valid := configDependencyMacroSnapshotCellIndex(cell)
	if !valid {
		return configDependencyMacroSnapshotCell{}, false
	}
	result, valid := configDependencyMacroSnapshotCellAt(int(transform[index]))
	return result, valid
}

func configDependencyMacroSnapshotApplyKnownTransform(
	transform configDependencyMacroSnapshotTransform,
	cell configDependencyMacroSnapshotCell,
) configDependencyMacroSnapshotCell {
	result, valid := configDependencyMacroSnapshotApplyTransform(transform, cell)
	if !valid {
		// Snapshot construction validates every externally supplied definition.
		// Reaching this fallback therefore indicates corrupt internal state. Keep
		// the result conservative without introducing a panic into analysis.
		return configDependencyMacroSnapshotCell{
			definition: configDependencyMacroUnknown,
			recorded:   true,
		}
	}
	return result
}

func configDependencyMacroSnapshotComposeTransforms(
	outer, inner configDependencyMacroSnapshotTransform,
) configDependencyMacroSnapshotTransform {
	var result configDependencyMacroSnapshotTransform
	for index, packedCell := range inner {
		if packedCell >= configDependencyMacroSnapshotCellCount {
			result[index] = configDependencyMacroSnapshotConservativeCellIndex
			continue
		}
		result[index] = outer[packedCell]
		if result[index] >= configDependencyMacroSnapshotCellCount {
			result[index] = configDependencyMacroSnapshotConservativeCellIndex
		}
	}
	return result
}

func configDependencyMacroSnapshotJoinCells(
	left, right configDependencyMacroSnapshotCell,
) configDependencyMacroSnapshotCell {
	definition := configDependencyMacroUnknown
	if left.definition == right.definition {
		definition = left.definition
	}
	return configDependencyMacroSnapshotCell{
		definition: definition,
		recorded:   left.recorded || right.recorded,
	}
}

func configDependencyMacroSnapshotJoinTransforms(
	left, right configDependencyMacroSnapshotTransform,
) configDependencyMacroSnapshotTransform {
	var result configDependencyMacroSnapshotTransform
	for index := range result {
		result[index] = configDependencyMacroSnapshotJoinPackedCells(left[index], right[index])
	}
	return result
}

func configDependencyMacroSnapshotJoinPackedCells(left, right uint8) uint8 {
	if left >= configDependencyMacroSnapshotCellCount {
		left = configDependencyMacroSnapshotConservativeCellIndex
	}
	if right >= configDependencyMacroSnapshotCellCount {
		right = configDependencyMacroSnapshotConservativeCellIndex
	}
	leftDefinition, rightDefinition := left/2, right/2
	definition := uint8(0)
	if leftDefinition == rightDefinition {
		definition = leftDefinition
	}
	return 2*definition + (left|right)&1
}

func configDependencyMacroSnapshotJoinLeftFallbackTransform(
	fallback configDependencyMacroSnapshotCell,
) configDependencyMacroSnapshotTransform {
	var result configDependencyMacroSnapshotTransform
	packedFallback := configDependencyMacroSnapshotPackCell(fallback)
	for index := range result {
		result[index] = configDependencyMacroSnapshotJoinPackedCells(packedFallback, uint8(index))
	}
	return result
}

func configDependencyMacroSnapshotJoinRightFallbackTransform(
	fallback configDependencyMacroSnapshotCell,
) configDependencyMacroSnapshotTransform {
	var result configDependencyMacroSnapshotTransform
	packedFallback := configDependencyMacroSnapshotPackCell(fallback)
	for index := range result {
		result[index] = configDependencyMacroSnapshotJoinPackedCells(uint8(index), packedFallback)
	}
	return result
}

// configDependencyMacroSnapshotTree is an immutable deterministic treap. A
// deterministic priority gives independently constructed snapshots the same
// shape for the same key set, while the key tie-break keeps priority ordering
// total even if the 64-bit digest prefix collides. Collisions can affect only
// balance, never lookup correctness.
type configDependencyMacroSnapshotTree struct {
	name     string
	cell     configDependencyMacroSnapshotCell
	priority uint64
	left     configDependencyMacroSnapshotTreeView
	right    configDependencyMacroSnapshotTreeView
}

// A view applies transform lazily to every cell below root. Child views carry
// their own tags, so an exact write pushes a namespace-wide transform only
// along the changed search path and continues sharing every untouched subtree.
type configDependencyMacroSnapshotTreeView struct {
	root      *configDependencyMacroSnapshotTree
	transform configDependencyMacroSnapshotTransform
}

func configDependencyMacroSnapshotTreePriority(name string) uint64 {
	digest := sha256.Sum256([]byte(name))
	return binary.BigEndian.Uint64(digest[:8])
}

func configDependencyMacroSnapshotTreePriorityGreater(
	leftPriority uint64,
	leftName string,
	rightPriority uint64,
	rightName string,
) bool {
	if leftPriority != rightPriority {
		return leftPriority > rightPriority
	}
	return leftName > rightName
}

func configDependencyMacroSnapshotIdentityTreeView(
	root *configDependencyMacroSnapshotTree,
) configDependencyMacroSnapshotTreeView {
	if root == nil {
		return configDependencyMacroSnapshotTreeView{}
	}
	return configDependencyMacroSnapshotTreeView{
		root:      root,
		transform: configDependencyMacroSnapshotIdentityTransform(),
	}
}

func configDependencyMacroSnapshotTransformTree(
	tree configDependencyMacroSnapshotTreeView,
	transform configDependencyMacroSnapshotTransform,
) configDependencyMacroSnapshotTreeView {
	if tree.root == nil {
		return configDependencyMacroSnapshotTreeView{}
	}
	// Exact writes leave identity views along their copied search paths. This
	// common composition needs no six-cell work. Require both tags to be the
	// identity: a general identity-outer shortcut would skip the conservative
	// normalization of invalid inner packed cells performed below.
	identity := configDependencyMacroSnapshotIdentityTransform()
	if transform == identity && tree.transform == identity {
		return tree
	}
	composed := configDependencyMacroSnapshotComposeTransforms(transform, tree.transform)
	if composed == tree.transform {
		return tree
	}
	return configDependencyMacroSnapshotTreeView{root: tree.root, transform: composed}
}

type configDependencyMacroSnapshotExposedTree struct {
	name     string
	cell     configDependencyMacroSnapshotCell
	priority uint64
	left     configDependencyMacroSnapshotTreeView
	right    configDependencyMacroSnapshotTreeView
}

func configDependencyMacroSnapshotExposeTree(
	tree configDependencyMacroSnapshotTreeView,
) (configDependencyMacroSnapshotExposedTree, bool) {
	if tree.root == nil {
		return configDependencyMacroSnapshotExposedTree{}, false
	}
	return configDependencyMacroSnapshotExposedTree{
		name:     tree.root.name,
		cell:     configDependencyMacroSnapshotApplyKnownTransform(tree.transform, tree.root.cell),
		priority: tree.root.priority,
		left:     configDependencyMacroSnapshotTransformTree(tree.root.left, tree.transform),
		right:    configDependencyMacroSnapshotTransformTree(tree.root.right, tree.transform),
	}, true
}

func configDependencyMacroSnapshotNewTree(
	name string,
	cell configDependencyMacroSnapshotCell,
	priority uint64,
	left, right configDependencyMacroSnapshotTreeView,
) configDependencyMacroSnapshotTreeView {
	root := &configDependencyMacroSnapshotTree{
		name:     name,
		cell:     cell,
		priority: priority,
		left:     left,
		right:    right,
	}
	return configDependencyMacroSnapshotIdentityTreeView(root)
}

func configDependencyMacroSnapshotTreeGet(
	tree configDependencyMacroSnapshotTreeView,
	name string,
) (configDependencyMacroSnapshotCell, bool) {
	root, transform := tree.root, tree.transform
	identity := configDependencyMacroSnapshotIdentityTransform()
	for root != nil {
		// A lookup only observes the matching cell and the chosen child. Exposing
		// each ancestor also transforms its cell and its unvisited sibling, work
		// needed by structural edits but not by this read-only search.
		var child configDependencyMacroSnapshotTreeView
		switch {
		case name < root.name:
			child = root.left
		case name > root.name:
			child = root.right
		default:
			return configDependencyMacroSnapshotApplyKnownTransform(transform, root.cell), true
		}
		// Skipping an identity composition here need not publish a normalized
		// view: any invalid packed entry is still normalized by the next actual
		// composition or the final ApplyKnownTransform. No tree is changed.
		if transform == identity {
			transform = child.transform
		} else if child.transform != identity {
			transform = configDependencyMacroSnapshotComposeTransforms(transform, child.transform)
		}
		root = child.root
	}
	return configDependencyMacroSnapshotCell{}, false
}

// split removes name when present and returns the two ordered persistent
// halves. Untouched paths retain their original view, including any lazy tag.
func configDependencyMacroSnapshotSplitTree(
	tree configDependencyMacroSnapshotTreeView,
	name string,
) (
	configDependencyMacroSnapshotTreeView,
	configDependencyMacroSnapshotCell,
	bool,
	configDependencyMacroSnapshotTreeView,
) {
	if tree.root == nil {
		return configDependencyMacroSnapshotTreeView{}, configDependencyMacroSnapshotCell{}, false,
			configDependencyMacroSnapshotTreeView{}
	}
	exposed, _ := configDependencyMacroSnapshotExposeTree(tree)
	switch {
	case name < exposed.name:
		less, cell, found, greaterLeft := configDependencyMacroSnapshotSplitTree(exposed.left, name)
		if greaterLeft == exposed.left {
			return less, cell, found, tree
		}
		greater := configDependencyMacroSnapshotNewTree(
			exposed.name, exposed.cell, exposed.priority, greaterLeft, exposed.right,
		)
		return less, cell, found, greater
	case name > exposed.name:
		lessRight, cell, found, greater := configDependencyMacroSnapshotSplitTree(exposed.right, name)
		if lessRight == exposed.right {
			return tree, cell, found, greater
		}
		less := configDependencyMacroSnapshotNewTree(
			exposed.name, exposed.cell, exposed.priority, exposed.left, lessRight,
		)
		return less, cell, found, greater
	default:
		return exposed.left, exposed.cell, true, exposed.right
	}
}

// merge requires every key in left to sort before every key in right.
func configDependencyMacroSnapshotMergeTrees(
	left, right configDependencyMacroSnapshotTreeView,
) configDependencyMacroSnapshotTreeView {
	if left.root == nil {
		return right
	}
	if right.root == nil {
		return left
	}
	leftRoot, _ := configDependencyMacroSnapshotExposeTree(left)
	rightRoot, _ := configDependencyMacroSnapshotExposeTree(right)
	if configDependencyMacroSnapshotTreePriorityGreater(
		leftRoot.priority, leftRoot.name, rightRoot.priority, rightRoot.name,
	) {
		mergedRight := configDependencyMacroSnapshotMergeTrees(leftRoot.right, right)
		if mergedRight == leftRoot.right {
			return left
		}
		return configDependencyMacroSnapshotNewTree(
			leftRoot.name, leftRoot.cell, leftRoot.priority, leftRoot.left, mergedRight,
		)
	}
	mergedLeft := configDependencyMacroSnapshotMergeTrees(left, rightRoot.left)
	if mergedLeft == rightRoot.left {
		return right
	}
	return configDependencyMacroSnapshotNewTree(
		rightRoot.name, rightRoot.cell, rightRoot.priority, mergedLeft, rightRoot.right,
	)
}

func configDependencyMacroSnapshotPutTree(
	tree configDependencyMacroSnapshotTreeView,
	name string,
	cell, fallback configDependencyMacroSnapshotCell,
) configDependencyMacroSnapshotTreeView {
	priority := configDependencyMacroSnapshotTreePriority(name)
	var update func(configDependencyMacroSnapshotTreeView) configDependencyMacroSnapshotTreeView
	update = func(current configDependencyMacroSnapshotTreeView) configDependencyMacroSnapshotTreeView {
		if current.root == nil {
			if cell == fallback {
				return current
			}
			return configDependencyMacroSnapshotNewTree(
				name, cell, priority,
				configDependencyMacroSnapshotTreeView{}, configDependencyMacroSnapshotTreeView{},
			)
		}
		exposed, _ := configDependencyMacroSnapshotExposeTree(current)
		if name == exposed.name {
			if cell == exposed.cell {
				return current
			}
			if cell == fallback {
				return configDependencyMacroSnapshotMergeTrees(exposed.left, exposed.right)
			}
			return configDependencyMacroSnapshotNewTree(
				exposed.name, cell, exposed.priority, exposed.left, exposed.right,
			)
		}
		if cell != fallback && configDependencyMacroSnapshotTreePriorityGreater(
			priority, name, exposed.priority, exposed.name,
		) {
			left, _, _, right := configDependencyMacroSnapshotSplitTree(current, name)
			return configDependencyMacroSnapshotNewTree(name, cell, priority, left, right)
		}
		if name < exposed.name {
			left := update(exposed.left)
			if left == exposed.left {
				return current
			}
			return configDependencyMacroSnapshotNewTree(
				exposed.name, exposed.cell, exposed.priority, left, exposed.right,
			)
		}
		right := update(exposed.right)
		if right == exposed.right {
			return current
		}
		return configDependencyMacroSnapshotNewTree(
			exposed.name, exposed.cell, exposed.priority, exposed.left, right,
		)
	}
	return update(tree)
}

func configDependencyMacroSnapshotWalkTree(
	tree configDependencyMacroSnapshotTreeView,
	visit func(string, configDependencyMacroSnapshotCell),
) {
	if tree.root == nil {
		return
	}
	exposed, _ := configDependencyMacroSnapshotExposeTree(tree)
	configDependencyMacroSnapshotWalkTree(exposed.left, visit)
	visit(exposed.name, exposed.cell)
	configDependencyMacroSnapshotWalkTree(exposed.right, visit)
}

func configDependencyMacroSnapshotJoinTrees(
	left, right configDependencyMacroSnapshotTreeView,
	leftFallback, rightFallback, resultFallback configDependencyMacroSnapshotCell,
) configDependencyMacroSnapshotTreeView {
	if left.root == nil {
		return configDependencyMacroSnapshotTransformTree(
			right,
			configDependencyMacroSnapshotJoinLeftFallbackTransform(leftFallback),
		)
	}
	if right.root == nil {
		return configDependencyMacroSnapshotTransformTree(
			left,
			configDependencyMacroSnapshotJoinRightFallbackTransform(rightFallback),
		)
	}
	if left.root == right.root {
		joined := configDependencyMacroSnapshotJoinTransforms(left.transform, right.transform)
		if joined == left.transform {
			return left
		}
		if joined == right.transform {
			return right
		}
		return configDependencyMacroSnapshotTreeView{root: left.root, transform: joined}
	}

	leftRoot, _ := configDependencyMacroSnapshotExposeTree(left)
	rightRoot, _ := configDependencyMacroSnapshotExposeTree(right)
	if configDependencyMacroSnapshotTreePriorityGreater(
		leftRoot.priority, leftRoot.name, rightRoot.priority, rightRoot.name,
	) {
		rightLeft, rightCell, rightFound, rightRight :=
			configDependencyMacroSnapshotSplitTree(right, leftRoot.name)
		joinedLeft := configDependencyMacroSnapshotJoinTrees(
			leftRoot.left, rightLeft, leftFallback, rightFallback, resultFallback,
		)
		joinedRight := configDependencyMacroSnapshotJoinTrees(
			leftRoot.right, rightRight, leftFallback, rightFallback, resultFallback,
		)
		if !rightFound {
			rightCell = rightFallback
		}
		joinedCell := configDependencyMacroSnapshotJoinCells(leftRoot.cell, rightCell)
		if joinedCell == resultFallback {
			return configDependencyMacroSnapshotMergeTrees(joinedLeft, joinedRight)
		}
		if joinedCell == leftRoot.cell && joinedLeft == leftRoot.left && joinedRight == leftRoot.right {
			return left
		}
		return configDependencyMacroSnapshotNewTree(
			leftRoot.name, joinedCell, leftRoot.priority, joinedLeft, joinedRight,
		)
	}

	leftLeft, leftCell, leftFound, leftRight :=
		configDependencyMacroSnapshotSplitTree(left, rightRoot.name)
	joinedLeft := configDependencyMacroSnapshotJoinTrees(
		leftLeft, rightRoot.left, leftFallback, rightFallback, resultFallback,
	)
	joinedRight := configDependencyMacroSnapshotJoinTrees(
		leftRight, rightRoot.right, leftFallback, rightFallback, resultFallback,
	)
	if !leftFound {
		leftCell = leftFallback
	}
	joinedCell := configDependencyMacroSnapshotJoinCells(leftCell, rightRoot.cell)
	if joinedCell == resultFallback {
		return configDependencyMacroSnapshotMergeTrees(joinedLeft, joinedRight)
	}
	if joinedCell == rightRoot.cell && joinedLeft == rightRoot.left && joinedRight == rightRoot.right {
		return right
	}
	return configDependencyMacroSnapshotNewTree(
		rightRoot.name, joinedCell, rightRoot.priority, joinedLeft, joinedRight,
	)
}

type configDependencyMacroSnapshotNamespace struct {
	fallback configDependencyMacroSnapshotCell
	facts    configDependencyMacroSnapshotTreeView
}

func (namespace configDependencyMacroSnapshotNamespace) lookup(
	name string,
) configDependencyMacroSnapshotCell {
	if cell, found := configDependencyMacroSnapshotTreeGet(namespace.facts, name); found {
		return cell
	}
	return namespace.fallback
}

func (namespace configDependencyMacroSnapshotNamespace) withCell(
	name string,
	cell configDependencyMacroSnapshotCell,
) configDependencyMacroSnapshotNamespace {
	namespace.facts = configDependencyMacroSnapshotPutTree(
		namespace.facts, name, cell, namespace.fallback,
	)
	return namespace
}

type configDependencyMacroSnapshotEntry struct {
	name string
	cell configDependencyMacroSnapshotCell
}

// withSortedCells is the narrow writer for already validated, sorted, unique
// effective changes. Sparse writes share the old tree; dense writes build a
// private Cartesian tree and publish it only after every node is complete.
func (namespace configDependencyMacroSnapshotNamespace) withSortedCells(
	changes []configDependencyMacroSnapshotEntry,
) configDependencyMacroSnapshotNamespace {
	if len(changes) == 0 {
		return namespace
	}
	pointWrites := func() configDependencyMacroSnapshotNamespace {
		result := namespace
		for _, change := range changes {
			result = result.withCell(change.name, change.cell)
		}
		return result
	}
	if len(changes) < 64 {
		return pointWrites()
	}

	// Count stored entries, including cells made redundant by lazy transforms.
	// Stopping above the density budget avoids walking a huge sparse namespace
	// just to decide that its existing persistent point writer is preferable.
	type priorEntry struct {
		configDependencyMacroSnapshotEntry
		priority uint64
	}
	var existing []priorEntry
	limit := 4 * len(changes)
	var collect func(configDependencyMacroSnapshotTreeView) bool
	collect = func(tree configDependencyMacroSnapshotTreeView) bool {
		if tree.root == nil {
			return true
		}
		exposed, _ := configDependencyMacroSnapshotExposeTree(tree)
		if !collect(exposed.left) || len(existing) == limit {
			return false
		}
		existing = append(existing, priorEntry{
			configDependencyMacroSnapshotEntry: configDependencyMacroSnapshotEntry{
				name: exposed.name, cell: exposed.cell,
			},
			priority: exposed.priority,
		})
		return collect(exposed.right)
	}
	if !collect(namespace.facts) {
		return pointWrites()
	}

	// The stack contains only newly allocated nodes. Do not use a node arena:
	// a later sparse branch must not retain an entire obsolete allocation via
	// one surviving node. Existing priorities are carried through the merge.
	var stack []*configDependencyMacroSnapshotTree
	appendCell := func(name string, cell configDependencyMacroSnapshotCell, priority uint64) {
		if cell == namespace.fallback {
			return
		}
		node := &configDependencyMacroSnapshotTree{name: name, cell: cell, priority: priority}
		var left *configDependencyMacroSnapshotTree
		for len(stack) != 0 {
			top := stack[len(stack)-1]
			if !configDependencyMacroSnapshotTreePriorityGreater(priority, name, top.priority, top.name) {
				break
			}
			left = top
			stack = stack[:len(stack)-1]
		}
		node.left = configDependencyMacroSnapshotIdentityTreeView(left)
		if len(stack) != 0 {
			stack[len(stack)-1].right = configDependencyMacroSnapshotIdentityTreeView(node)
		}
		stack = append(stack, node)
	}
	for oldIndex, changeIndex := 0, 0; oldIndex < len(existing) || changeIndex < len(changes); {
		if changeIndex == len(changes) || oldIndex < len(existing) && existing[oldIndex].name < changes[changeIndex].name {
			entry := existing[oldIndex]
			appendCell(entry.name, entry.cell, entry.priority)
			oldIndex++
			continue
		}
		change := changes[changeIndex]
		var priority uint64
		if oldIndex < len(existing) && existing[oldIndex].name == change.name {
			priority = existing[oldIndex].priority
			oldIndex++
		} else if change.cell != namespace.fallback {
			priority = configDependencyMacroSnapshotTreePriority(change.name)
		}
		appendCell(change.name, change.cell, priority)
		changeIndex++
	}
	namespace.facts = configDependencyMacroSnapshotTreeView{}
	if len(stack) != 0 {
		namespace.facts = configDependencyMacroSnapshotIdentityTreeView(stack[0])
	}
	return namespace
}

func (namespace configDependencyMacroSnapshotNamespace) transformAll(
	transform configDependencyMacroSnapshotTransform,
) configDependencyMacroSnapshotNamespace {
	namespace.fallback = configDependencyMacroSnapshotApplyKnownTransform(transform, namespace.fallback)
	namespace.facts = configDependencyMacroSnapshotTransformTree(namespace.facts, transform)
	return namespace
}

func configDependencyJoinMacroSnapshotNamespaces(
	left, right configDependencyMacroSnapshotNamespace,
) configDependencyMacroSnapshotNamespace {
	result := configDependencyMacroSnapshotNamespace{
		fallback: configDependencyMacroSnapshotJoinCells(left.fallback, right.fallback),
	}
	result.facts = configDependencyMacroSnapshotJoinTrees(
		left.facts, right.facts, left.fallback, right.fallback, result.fallback,
	)
	return result
}

type configDependencyMacroSnapshotNamespaceKind uint8

const (
	configDependencyMacroSnapshotOrdinaryNamespace configDependencyMacroSnapshotNamespaceKind = iota
	configDependencyMacroSnapshotConfigNamespace
	configDependencyMacroSnapshotReservedNamespace
)

func configDependencyMacroSnapshotNamespaceForName(
	name string,
) configDependencyMacroSnapshotNamespaceKind {
	if strings.HasPrefix(name, "CONFIG_") {
		return configDependencyMacroSnapshotConfigNamespace
	}
	if strings.HasPrefix(name, "_") {
		return configDependencyMacroSnapshotReservedNamespace
	}
	return configDependencyMacroSnapshotOrdinaryNamespace
}

// configDependencyMacroSnapshot is an immutable complete macro namespace.
// A mutable preprocessor state can replace one pointer on each mutation;
// branch and clone operations share the pointer, and branch joins preserve all
// equal subtrees. projectionOnce is safe because constructors below never copy
// a snapshot containing an initialized sync.Once.
type configDependencyMacroSnapshot struct {
	config   configDependencyMacroSnapshotNamespace
	ordinary configDependencyMacroSnapshotNamespace
	reserved configDependencyMacroSnapshotNamespace

	lookupMemo      sync.Map
	projectionOnce  sync.Once
	projection      configDependencyConditionalMacroStateProjection
	projectionValid bool
}

func newConfigDependencyMacroSnapshot() *configDependencyMacroSnapshot {
	absent := configDependencyMacroSnapshotCell{
		definition: configDependencyMacroUndefined,
	}
	reservedAbsent := configDependencyMacroSnapshotCell{
		definition: configDependencyMacroUnknown,
	}
	return &configDependencyMacroSnapshot{
		config:   configDependencyMacroSnapshotNamespace{fallback: absent},
		ordinary: configDependencyMacroSnapshotNamespace{fallback: absent},
		reserved: configDependencyMacroSnapshotNamespace{fallback: reservedAbsent},
	}
}

func (snapshot *configDependencyMacroSnapshot) namespace(
	kind configDependencyMacroSnapshotNamespaceKind,
) configDependencyMacroSnapshotNamespace {
	switch kind {
	case configDependencyMacroSnapshotConfigNamespace:
		return snapshot.config
	case configDependencyMacroSnapshotReservedNamespace:
		return snapshot.reserved
	default:
		return snapshot.ordinary
	}
}

func (snapshot *configDependencyMacroSnapshot) withNamespace(
	kind configDependencyMacroSnapshotNamespaceKind,
	namespace configDependencyMacroSnapshotNamespace,
) *configDependencyMacroSnapshot {
	if snapshot == nil {
		return nil
	}
	if namespace == snapshot.namespace(kind) {
		return snapshot
	}
	result := &configDependencyMacroSnapshot{
		config:   snapshot.config,
		ordinary: snapshot.ordinary,
		reserved: snapshot.reserved,
	}
	switch kind {
	case configDependencyMacroSnapshotConfigNamespace:
		result.config = namespace
	case configDependencyMacroSnapshotReservedNamespace:
		result.reserved = namespace
	default:
		result.ordinary = namespace
	}
	return result
}

func (snapshot *configDependencyMacroSnapshot) lookup(
	name string,
) (configDependencyMacroSnapshotCell, bool) {
	if snapshot == nil {
		return configDependencyMacroSnapshotCell{}, false
	}
	if cached, found := snapshot.lookupMemo.Load(name); found {
		cell, valid := cached.(configDependencyMacroSnapshotCell)
		if valid {
			return cell, true
		}
	}
	kind := configDependencyMacroSnapshotNamespaceForName(name)
	cell := snapshot.namespace(kind).lookup(name)
	snapshot.lookupMemo.Store(name, cell)
	return cell, true
}

// withCell is the primitive used by direct writes and exhaustive semantic
// tests. Callers which model a real #define/#undef should use withDefinition so
// the explicit write is always recorded.
func (snapshot *configDependencyMacroSnapshot) withCell(
	name string,
	cell configDependencyMacroSnapshotCell,
) (*configDependencyMacroSnapshot, bool) {
	if snapshot == nil || name == "" {
		return nil, false
	}
	if _, valid := configDependencyMacroSnapshotCellIndex(cell); !valid {
		return nil, false
	}
	kind := configDependencyMacroSnapshotNamespaceForName(name)
	return snapshot.withNamespace(kind, snapshot.namespace(kind).withCell(name, cell)), true
}

func (snapshot *configDependencyMacroSnapshot) withDefinition(
	name string,
	definition configDependencyMacroDefinition,
) (*configDependencyMacroSnapshot, bool) {
	return snapshot.withCell(name, configDependencyMacroSnapshotCell{
		definition: definition,
		recorded:   true,
	})
}

// withValidatedNumericMacroHeader applies the exact validator guarantee: an
// unavailable numeric header may define any non-CONFIG name but cannot
// undefine one. CONFIG names are untouched. The transform is lazy over both
// non-CONFIG namespaces and therefore independent of namespace size.
func (snapshot *configDependencyMacroSnapshot) withValidatedNumericMacroHeader() (
	*configDependencyMacroSnapshot,
	bool,
) {
	if snapshot == nil {
		return nil, false
	}
	transform := configDependencyMacroSnapshotMaybeDefineTransform()
	ordinary := snapshot.ordinary.transformAll(transform)
	reserved := snapshot.reserved.transformAll(transform)
	if ordinary == snapshot.ordinary && reserved == snapshot.reserved {
		return snapshot, true
	}
	return &configDependencyMacroSnapshot{
		config:   snapshot.config,
		ordinary: ordinary,
		reserved: reserved,
	}, true
}

func (snapshot *configDependencyMacroSnapshot) withConfigNameSet(
	names []string,
	maybe bool,
) (*configDependencyMacroSnapshot, bool) {
	if snapshot == nil {
		return nil, false
	}
	for _, name := range names {
		if name == "" || !strings.HasPrefix(name, "CONFIG_") {
			return nil, false
		}
	}
	namespace := snapshot.config
	defined := configDependencyMacroSnapshotCell{
		definition: configDependencyMacroDefined,
		recorded:   true,
	}
	for _, name := range names {
		cell := defined
		if maybe {
			cell = configDependencyMacroSnapshotMaybeDefineCell(namespace.lookup(name))
		}
		namespace = namespace.withCell(name, cell)
	}
	return snapshot.withNamespace(configDependencyMacroSnapshotConfigNamespace, namespace), true
}

func (snapshot *configDependencyMacroSnapshot) withConfigDefinitions(
	names []string,
) (*configDependencyMacroSnapshot, bool) {
	return snapshot.withConfigNameSet(names, false)
}

func (snapshot *configDependencyMacroSnapshot) withMaybeConfigDefinitions(
	names []string,
) (*configDependencyMacroSnapshot, bool) {
	return snapshot.withConfigNameSet(names, true)
}

func configDependencyJoinMacroSnapshots(
	left, right *configDependencyMacroSnapshot,
) (*configDependencyMacroSnapshot, bool) {
	if left == nil || right == nil {
		return nil, false
	}
	if left == right {
		return left, true
	}
	result := &configDependencyMacroSnapshot{
		config:   configDependencyJoinMacroSnapshotNamespaces(left.config, right.config),
		ordinary: configDependencyJoinMacroSnapshotNamespaces(left.ordinary, right.ordinary),
		reserved: configDependencyJoinMacroSnapshotNamespaces(left.reserved, right.reserved),
	}
	if result.config == left.config && result.ordinary == left.ordinary && result.reserved == left.reserved {
		return left, true
	}
	if result.config == right.config && result.ordinary == right.ordinary && result.reserved == right.reserved {
		return right, true
	}
	return result, true
}

// conditionalProjection materializes the same collision-free definition-only
// view consumed by recursive-include progress checks. The recorded bit remains
// available through lookup but deliberately does not participate in that
// existing key contract. Returned facts are immutable and shared by subsequent
// calls on the same snapshot.
func (snapshot *configDependencyMacroSnapshot) conditionalProjection() (
	configDependencyConditionalMacroStateProjection,
	bool,
) {
	if snapshot == nil {
		return configDependencyConditionalMacroStateProjection{}, false
	}
	snapshot.projectionOnce.Do(func() {
		if snapshot.config.fallback.definition != configDependencyMacroUndefined ||
			snapshot.ordinary.fallback.definition != configDependencyMacroUndefined &&
				snapshot.ordinary.fallback.definition != configDependencyMacroUnknown ||
			snapshot.reserved.fallback.definition != configDependencyMacroUnknown {
			return
		}
		facts := map[string]configDependencyMacroDefinition{}
		for _, namespace := range []configDependencyMacroSnapshotNamespace{
			snapshot.config, snapshot.ordinary, snapshot.reserved,
		} {
			configDependencyMacroSnapshotWalkTree(namespace.facts, func(
				name string,
				cell configDependencyMacroSnapshotCell,
			) {
				if cell.definition != namespace.fallback.definition {
					facts[name] = cell.definition
				}
			})
		}
		names := make([]string, 0, len(facts))
		for name := range facts {
			names = append(names, name)
		}
		sort.Strings(names)
		unknownNonConfig := snapshot.ordinary.fallback.definition == configDependencyMacroUnknown
		var key strings.Builder
		if unknownNonConfig {
			key.WriteString("W1;")
		} else {
			key.WriteString("W0;")
		}
		for _, name := range names {
			definition := facts[name]
			key.WriteString(strconv.Itoa(len(name)))
			key.WriteByte(':')
			key.WriteString(name)
			key.WriteByte('=')
			switch definition {
			case configDependencyMacroUndefined:
				key.WriteByte('0')
			case configDependencyMacroDefined:
				key.WriteByte('1')
			case configDependencyMacroUnknown:
				key.WriteByte('?')
			default:
				return
			}
			key.WriteByte(';')
		}
		snapshot.projection = configDependencyConditionalMacroStateProjection{
			key:              key.String(),
			facts:            facts,
			unknownNonConfig: unknownNonConfig,
		}
		snapshot.projectionValid = true
	})
	return snapshot.projection, snapshot.projectionValid
}
