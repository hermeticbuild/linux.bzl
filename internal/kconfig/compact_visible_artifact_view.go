package kconfig

// CompactKbuildInitialVisibleArtifactView is the process-local, immutable view
// of the object-tree frontier visible when one concrete Make invocation
// started. Get resolves one exact canonical path. Range visits matching paths
// in strictly increasing order; an empty prefix visits the complete frontier.
// Returning false from visit stops iteration.
//
// Implementations must remain immutable for the lifetime of every profile
// which references them. The interface is intentionally not serialized: the
// action plan records only the exact artifact subsets consumed by selected
// actions.
type CompactKbuildInitialVisibleArtifactView interface {
	Len() int
	Get(path string) (CompactKbuildVisibleArtifact, bool)
	Range(prefix string, visit func(CompactKbuildVisibleArtifact) bool)
}

// SetCompactKbuildProfileInitialVisibleArtifactView installs a process-local
// frontier. An installed view is a canonical-validity contract: Len, Get, and
// Range must describe the same
// immutable mapping; Range must return strictly increasing, unique canonical
// paths and exact prefix subsets. Profile and selection validation deliberately
// do not exhaustively enumerate an installed view. Artifacts actually selected
// by the action graph are still checked through Get.
//
// Supplying nil clears the view.
func SetCompactKbuildProfileInitialVisibleArtifactView(
	profile *CompactKbuildProfile,
	view CompactKbuildInitialVisibleArtifactView,
) {
	if profile == nil {
		return
	}
	profile.initialVisibleArtifactView = view
}

// CompactKbuildProfileInitialVisibleArtifactCount returns the number of exact
// path owners in profile's initial object-tree frontier.
func CompactKbuildProfileInitialVisibleArtifactCount(profile CompactKbuildProfile) int {
	if profile.initialVisibleArtifactView == nil {
		return 0
	}
	return profile.initialVisibleArtifactView.Len()
}

// CompactKbuildProfileInitialVisibleArtifact resolves one exact path owner.
func CompactKbuildProfileInitialVisibleArtifact(
	profile CompactKbuildProfile,
	path string,
) (CompactKbuildVisibleArtifact, bool) {
	if profile.initialVisibleArtifactView == nil {
		return CompactKbuildVisibleArtifact{}, false
	}
	artifact, ok := profile.initialVisibleArtifactView.Get(path)
	if !ok || artifact.Path != path {
		return CompactKbuildVisibleArtifact{}, false
	}
	return artifact, true
}

// RangeCompactKbuildProfileInitialVisibleArtifacts visits profile's initial
// path owners in sorted order. A non-empty prefix limits iteration to the
// lexical prefix; an empty prefix visits the complete frontier.
func RangeCompactKbuildProfileInitialVisibleArtifacts(
	profile CompactKbuildProfile,
	prefix string,
	visit func(CompactKbuildVisibleArtifact) bool,
) {
	if visit == nil || profile.initialVisibleArtifactView == nil {
		return
	}
	profile.initialVisibleArtifactView.Range(prefix, visit)
}
