package kconfig

import "strings"

// configDependencySourceLookup is an immutable, process-local snapshot of the
// exact invocation's source namespace. Scanners borrow it without retaining
// the Make evaluator or its mutable rule indexes. Physical roots come only
// from the owning profile; this is not an import API or a source-closure proof.
type configDependencySourceLookup struct {
	profileName string
	templateSet bool
	bindings    map[string]compactKbuildSourceRootBinding
	location    CompactKbuildInvocationLocation
	locationSet bool
	overlayRoot string
	overlayErr  error
}

func newConfigDependencySourceLookup(profile CompactKbuildProfile) configDependencySourceLookup {
	lookup := configDependencySourceLookup{profileName: profile.Name}
	lookup.location, lookup.locationSet = CompactKbuildProfileInvocationLocation(profile)
	if profile.evaluator != nil && profile.evaluator.template != nil {
		lookup.templateSet = true
		lookup.bindings = compactKbuildProfileSourceRootBindings(profile.evaluator.template.sourceRoots)
	}
	// Preserve the distinction between an explicit nested source-tree overlay
	// and a direct configured source root with the same logical prefix. Invalid
	// nested markers also retain their original fail-closed overlay diagnostic.
	lookup.overlayRoot, _, lookup.overlayErr = compactKbuildSourceOverlayRoot(profile)
	return lookup
}

func (lookup configDependencySourceLookup) sourcePath(logical string) (string, bool) {
	if !lookup.templateSet {
		return "", false
	}
	logical = canonicalKbuildRulePath(logical)
	if validatePlanRelativePath("Kbuild source path", logical) != nil {
		return "", false
	}
	return resolveCompactKbuildSourcePath(lookup.bindings, logical)
}

func (lookup configDependencySourceLookup) usesSourceOverlay(logical string) (bool, error) {
	if lookup.overlayErr != nil || lookup.overlayRoot == "" {
		return false, lookup.overlayErr
	}
	logical = compactKbuildGraphTargetPath(logical)
	return logical != "" && (logical == lookup.overlayRoot || strings.HasPrefix(logical, lookup.overlayRoot+"/")), nil
}

func (lookup configDependencySourceLookup) includePath(value string) (CompactKbuildInvocationLocation, bool, bool, error) {
	return resolveCompactKbuildCompilerIncludePath(lookup.location, lookup.locationSet, value)
}
