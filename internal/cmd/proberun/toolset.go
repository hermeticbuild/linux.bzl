package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

var errToolsetPathOutsideClosure = errors.New("toolset path is not identity-bound")

type toolsetArtifactBinding struct {
	canonical string
	path      string
	directory bool
	source    bool
}

// toolsetPathResolver binds stable manifest paths to the physical artifacts
// selected for this action. Producer and consumer actions may receive
// different Bazel path mappings, so durable probe results never retain the
// physical spelling.
type toolsetPathResolver struct {
	manifest       toolaction.KbuildToolsetManifest
	execroot       string
	exact          map[string]toolsetArtifactBinding
	ordered        []toolsetArtifactBinding
	projectionRoot string
	projections    map[string]string
}

type toolsetActionPathMapping struct {
	canonical       string
	physical        string
	allowDescendant bool
	allowRunfiles   bool
}

func loadToolsetPathResolver(execroot, scope, markerIdentity, manifestFilename string, anchors map[string]string) (*toolsetPathResolver, error) {
	if manifestFilename == "" {
		return nil, fmt.Errorf("probe has no %s toolset manifest", scope)
	}
	manifest, err := toolaction.ReadKbuildToolsetManifest(manifestFilename)
	if err != nil {
		return nil, err
	}
	if manifest.Scope != scope {
		return nil, fmt.Errorf("probe %s toolset manifest has scope %q", scope, manifest.Scope)
	}
	identity, err := manifest.Identity()
	if err != nil {
		return nil, err
	}
	if identity != markerIdentity {
		return nil, fmt.Errorf("probe %s toolset manifest identity is %q, want marker %q", scope, identity, markerIdentity)
	}
	for root := range anchors {
		if _, exists := manifest.Roots[root]; !exists {
			return nil, fmt.Errorf("probe %s toolset root anchors contain unknown root %q", scope, root)
		}
	}
	for root := range manifest.Roots {
		if _, exists := anchors[root]; !exists {
			return nil, fmt.Errorf("probe %s toolset root anchors omit manifest root %q", scope, root)
		}
	}
	resolver := &toolsetPathResolver{
		manifest:    manifest,
		execroot:    filepath.Clean(execroot),
		exact:       make(map[string]toolsetArtifactBinding, len(manifest.Closure)),
		projections: make(map[string]string),
	}
	physicalRoots := make(map[string]string, len(manifest.Roots))
	for root, anchorCanonical := range manifest.Roots {
		filename := anchors[root]
		canonical, err := canonicalActionArtifactPath(execroot, filename)
		if err != nil {
			return nil, fmt.Errorf("canonicalize %s toolset root %q anchor %q: %w", scope, root, filename, err)
		}
		if canonical != anchorCanonical {
			return nil, fmt.Errorf("probe %s toolset root %q anchor maps to canonical artifact %q, manifest binds %q", scope, root, canonical, anchorCanonical)
		}
		physicalAnchor := filepath.Clean(filename)
		if !filepath.IsAbs(physicalAnchor) {
			physicalAnchor = filepath.Join(resolver.execroot, physicalAnchor)
		}
		location := manifest.ArtifactRoots[anchorCanonical]
		physicalRoot, err := deriveToolsetPhysicalRoot(physicalAnchor, location.Path)
		if err != nil {
			return nil, fmt.Errorf("derive %s toolset root %q from anchor %q: %w", scope, root, filename, err)
		}
		physicalRoots[root] = physicalRoot
	}
	for _, canonical := range manifest.Closure {
		location := manifest.ArtifactRoots[canonical]
		filename := filepath.Join(physicalRoots[location.Root], filepath.FromSlash(location.Path))
		gotCanonical, err := canonicalActionArtifactPath(execroot, filename)
		if err != nil {
			return nil, fmt.Errorf("canonicalize reconstructed %s toolset artifact %q: %w", scope, filename, err)
		}
		if gotCanonical != canonical {
			return nil, fmt.Errorf("reconstructed %s toolset artifact %q maps to canonical artifact %q, manifest binds %q", scope, filename, gotCanonical, canonical)
		}
		info, err := os.Stat(filename)
		if err != nil {
			return nil, fmt.Errorf("inspect %s toolset artifact %q: %w", scope, canonical, err)
		}
		kind := manifest.ArtifactKinds[canonical]
		source := kind == toolaction.KbuildToolsetArtifactSource
		// Bazel analysis cannot distinguish a legacy opaque source directory
		// from a source file. Inspecting the exact identity-bound SourceArtifact
		// is safe: it grants no authority over an inferred ancestor or sibling.
		directory := kind == toolaction.KbuildToolsetArtifactGeneratedDirectory
		if source {
			directory = info.IsDir()
		}
		if !source && info.IsDir() != directory {
			return nil, fmt.Errorf("probe %s toolset artifact %q is %s on disk, manifest binds kind %q", scope, canonical, map[bool]string{true: "a directory", false: "not a directory"}[info.IsDir()], kind)
		}
		binding := toolsetArtifactBinding{
			canonical: canonical,
			path:      filepath.Clean(filename),
			directory: directory,
			source:    source,
		}
		resolver.exact[canonical] = binding
		resolver.ordered = append(resolver.ordered, binding)
	}
	sort.Slice(resolver.ordered, func(i, j int) bool {
		left, right := resolver.ordered[i].canonical, resolver.ordered[j].canonical
		if len(left) != len(right) {
			return len(left) > len(right)
		}
		return left < right
	})
	return resolver, nil
}

func deriveToolsetPhysicalRoot(anchor, relative string) (string, error) {
	root := filepath.Clean(anchor)
	for range strings.Split(relative, "/") {
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("root-relative path %q has more components than its physical anchor", relative)
		}
		root = parent
	}
	if reconstructed := filepath.Join(root, filepath.FromSlash(relative)); filepath.Clean(reconstructed) != filepath.Clean(anchor) {
		return "", fmt.Errorf("root-relative path %q does not identify its physical anchor", relative)
	}
	return root, nil
}

func (r *toolsetPathResolver) setProjectionRoot(root string) error {
	if root == "" || !filepath.IsAbs(root) {
		return fmt.Errorf("toolset projection root must be an absolute path")
	}
	if r.projectionRoot != "" && filepath.Clean(root) != r.projectionRoot {
		return fmt.Errorf("toolset projection root is already configured")
	}
	root = filepath.Clean(root)
	if err := os.Mkdir(root, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create toolset projection root: %w", err)
	}
	r.projectionRoot = root
	return nil
}

func canonicalActionArtifactPath(execroot, filename string) (string, error) {
	if filename == "" {
		return "", fmt.Errorf("empty artifact path")
	}
	absolute := filename
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(execroot, absolute)
	}
	relative, err := filepath.Rel(filepath.Clean(execroot), filepath.Clean(absolute))
	if err != nil {
		return "", err
	}
	return toolaction.CanonicalArtifactPath(filepath.ToSlash(relative))
}

func (r *toolsetPathResolver) verifyTool(role, filename string) error {
	want := r.manifest.Tools[role]
	if want == "" {
		return fmt.Errorf("configured tool role %q is absent from the %s toolset manifest", role, r.manifest.Scope)
	}
	got, err := canonicalActionArtifactPath(r.execroot, filename)
	if err != nil {
		return fmt.Errorf("configured tool role %q: %w", role, err)
	}
	if got != want {
		return fmt.Errorf("configured tool role %q resolves to %q, manifest selects %q", role, got, want)
	}
	resolved, err := r.resolve(want)
	if err != nil {
		return fmt.Errorf("configured tool role %q: %w", role, err)
	}
	wantPhysical, err := filepath.EvalSymlinks(filename)
	if err != nil {
		return fmt.Errorf("resolve configured tool role %q: %w", role, err)
	}
	gotPhysical, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return fmt.Errorf("resolve manifest tool role %q: %w", role, err)
	}
	if filepath.Clean(gotPhysical) != filepath.Clean(wantPhysical) {
		return fmt.Errorf("configured tool role %q is not its typed manifest artifact", role)
	}
	return nil
}

// verifyContract proves that the callback-supplied action envelope is exactly
// the one covered by the manifest identity. Bazel renders typed artifact
// fragments with this action's physical path mapping; canonicalActionValue
// reverses only those identity-bound fragments before comparison.
func (r *toolsetPathResolver) verifyContract(role string, contract actionContract) error {
	wantArguments, exists := r.manifest.Actions[role]
	if !exists {
		return fmt.Errorf("configured action role %q is absent from the %s toolset manifest", role, r.manifest.Scope)
	}
	gotArguments := make([]string, len(contract.arguments))
	for index, argument := range contract.arguments {
		canonical, err := r.canonicalActionValue(argument)
		if err != nil {
			return fmt.Errorf("configured action role %q argument %d: %w", role, index, err)
		}
		gotArguments[index] = canonical
	}
	if !slices.Equal(gotArguments, wantArguments) {
		return fmt.Errorf("configured action role %q arguments do not match the identity-bound %s toolset manifest", role, r.manifest.Scope)
	}
	wantEnvironment := r.manifest.Environments[role]
	if len(contract.environment) != len(wantEnvironment) {
		return fmt.Errorf("configured action role %q environment does not match the identity-bound %s toolset manifest", role, r.manifest.Scope)
	}
	for name, value := range contract.environment {
		want, exists := wantEnvironment[name]
		if !exists {
			return fmt.Errorf("configured action role %q environment name %q is absent from the identity-bound %s toolset manifest", role, name, r.manifest.Scope)
		}
		canonical, err := r.canonicalActionValue(value)
		if err != nil {
			return fmt.Errorf("configured action role %q environment %q: %w", role, name, err)
		}
		if canonical != want {
			return fmt.Errorf("configured action role %q environment %q does not match the identity-bound %s toolset manifest", role, name, r.manifest.Scope)
		}
	}
	return nil
}

func (r *toolsetPathResolver) canonicalActionValue(value string) (string, error) {
	mappings, err := r.actionPathMappings()
	if err != nil {
		return "", err
	}
	var out strings.Builder
	cursor := 0
	for cursor < len(value) {
		selected := -1
		selectedStart := len(value)
		for index, mapping := range mappings {
			start := nextActionPathOccurrence(value, cursor, mapping)
			if start < 0 {
				continue
			}
			if start < selectedStart || start == selectedStart && selected >= 0 && len(mapping.physical) > len(mappings[selected].physical) {
				selected = index
				selectedStart = start
			}
		}
		if selected < 0 {
			out.WriteString(value[cursor:])
			break
		}
		mapping := mappings[selected]
		out.WriteString(value[cursor:selectedStart])
		out.WriteString(mapping.canonical)
		cursor = selectedStart + len(mapping.physical)
	}
	return out.String(), nil
}

func nextActionPathOccurrence(value string, cursor int, mapping toolsetActionPathMapping) int {
	for cursor <= len(value) {
		start := strings.Index(value[cursor:], mapping.physical)
		if start < 0 {
			return -1
		}
		start += cursor
		end := start + len(mapping.physical)
		if actionPathOccurrence(value, start, end, mapping.allowDescendant, mapping.allowRunfiles) {
			return start
		}
		cursor = start + 1
	}
	return -1
}

func actionPathOccurrence(value string, start, end int, allowDescendant, allowRunfiles bool) bool {
	const delimiters = " =,:;\t@"
	const trailingDelimiters = "/=,:; \t"
	if end < len(value) && !strings.ContainsRune(trailingDelimiters, rune(value[end])) {
		runfilesEnd := end + len(".runfiles")
		if !allowRunfiles || !strings.HasPrefix(value[end:], ".runfiles") || runfilesEnd < len(value) && !strings.ContainsRune(trailingDelimiters, rune(value[runfilesEnd])) {
			return false
		}
	}
	if end < len(value) && value[end] == '/' && !allowDescendant {
		return false
	}
	if start == 0 || strings.ContainsRune(delimiters, rune(value[start-1])) {
		return true
	}
	tokenStart := 0
	for index := 0; index < start; index++ {
		if strings.ContainsRune(delimiters, rune(value[index])) {
			tokenStart = index + 1
		}
	}
	optionPrefix := value[tokenStart:start]
	return strings.HasPrefix(optionPrefix, "-") && !strings.ContainsRune(optionPrefix, '/')
}

func (r *toolsetPathResolver) actionPathMappings() ([]toolsetActionPathMapping, error) {
	mappingByPhysical := map[string]toolsetActionPathMapping{}
	conflicting := map[string]bool{}
	record := func(canonical, physical string, allowDescendant, allowRunfiles bool) {
		physical = filepath.Clean(physical)
		if existing, ok := mappingByPhysical[physical]; ok {
			if existing.canonical != canonical {
				conflicting[physical] = true
				return
			}
			existing.allowDescendant = existing.allowDescendant || allowDescendant
			existing.allowRunfiles = existing.allowRunfiles || allowRunfiles
			mappingByPhysical[physical] = existing
			return
		}
		mappingByPhysical[physical] = toolsetActionPathMapping{
			canonical:       canonical,
			physical:        physical,
			allowDescendant: allowDescendant,
			allowRunfiles:   allowRunfiles,
		}
	}
	for _, binding := range r.ordered {
		record(binding.canonical, binding.path, binding.directory || binding.source, !binding.directory)
		canonical := binding.canonical
		physical := binding.path
		for strings.Contains(canonical, "/") {
			canonical = canonical[:strings.LastIndexByte(canonical, '/')]
			physical = filepath.Dir(physical)
			if canonical == "external" {
				break
			}
			if binding.source {
				record(canonical, physical, true, false)
			}
		}
	}
	mappings := make([]toolsetActionPathMapping, 0, len(mappingByPhysical))
	for physical, mapping := range mappingByPhysical {
		if conflicting[physical] {
			continue
		}
		mappings = append(mappings, mapping)
	}
	sort.Slice(mappings, func(i, j int) bool {
		if len(mappings[i].physical) != len(mappings[j].physical) {
			return len(mappings[i].physical) > len(mappings[j].physical)
		}
		return mappings[i].physical < mappings[j].physical
	})
	return mappings, nil
}

// resolve maps a canonical manifest path, a proven directory ancestor, or a
// descendant of a typed directory artifact into this action's physical input.
func (r *toolsetPathResolver) resolve(canonical string) (string, error) {
	if err := toolaction.ValidateCanonicalArtifactPath(canonical); err != nil {
		return "", fmt.Errorf("invalid canonical toolset path %q: %w", canonical, err)
	}
	if binding, ok := r.exact[canonical]; ok {
		return binding.path, nil
	}

	// TreeArtifacts and other typed directories authorize only their own
	// descendants. Longest-prefix ordering avoids ambiguity for nested trees.
	for _, binding := range r.ordered {
		if !binding.directory || !strings.HasPrefix(canonical, binding.canonical+"/") {
			continue
		}
		candidate := filepath.Join(binding.path, filepath.FromSlash(strings.TrimPrefix(canonical, binding.canonical+"/")))
		if err := ensurePathInsideDirectory(binding.path, candidate); err != nil {
			return "", err
		}
		return candidate, nil
	}

	return r.projectCanonicalDirectory(canonical)
}

func (r *toolsetPathResolver) projectCanonicalDirectory(canonical string) (string, error) {
	if projected := r.projections[canonical]; projected != "" {
		return projected, nil
	}
	prefix := canonical + "/"
	bindings := make([]toolsetArtifactBinding, 0)
	for _, binding := range r.ordered {
		if strings.HasPrefix(binding.canonical, prefix) {
			bindings = append(bindings, binding)
		}
	}
	if len(bindings) == 0 {
		return "", fmt.Errorf("canonical path %q is outside the identity-bound %s toolset closure: %w", canonical, r.manifest.Scope, errToolsetPathOutsideClosure)
	}
	if r.projectionRoot == "" {
		return "", fmt.Errorf("canonical toolset directory %q requires an unconfigured private projection", canonical)
	}
	sort.Slice(bindings, func(i, j int) bool {
		if len(bindings[i].canonical) != len(bindings[j].canonical) {
			return len(bindings[i].canonical) < len(bindings[j].canonical)
		}
		return bindings[i].canonical < bindings[j].canonical
	})
	selected := make([]toolsetArtifactBinding, 0, len(bindings))
	for _, binding := range bindings {
		covered := false
		for _, prior := range selected {
			if prior.directory && strings.HasPrefix(binding.canonical, prior.canonical+"/") {
				covered = true
				break
			}
		}
		if !covered {
			selected = append(selected, binding)
		}
	}
	projection := filepath.Join(r.projectionRoot, filepath.FromSlash(canonical))
	if !probePhysicalPathWithin(r.projectionRoot, projection) {
		return "", fmt.Errorf("canonical toolset projection %q escapes its private root", canonical)
	}
	if err := os.MkdirAll(projection, 0o700); err != nil {
		return "", fmt.Errorf("create canonical toolset projection %q: %w", canonical, err)
	}
	for _, binding := range selected {
		suffix := strings.TrimPrefix(binding.canonical, prefix)
		destination := filepath.Join(projection, filepath.FromSlash(suffix))
		if !probePhysicalPathWithin(projection, destination) {
			return "", fmt.Errorf("toolset artifact %q escapes canonical projection %q", binding.canonical, canonical)
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return "", fmt.Errorf("create canonical toolset projection parent for %q: %w", binding.canonical, err)
		}
		physical, err := filepath.EvalSymlinks(binding.path)
		if err != nil {
			return "", fmt.Errorf("resolve canonical toolset artifact %q: %w", binding.canonical, err)
		}
		if err := materializeProjectionSymlink(destination, physical); err != nil {
			return "", fmt.Errorf("project canonical toolset artifact %q: %w", binding.canonical, err)
		}
	}
	r.projections[canonical] = projection
	return projection, nil
}

func materializeProjectionSymlink(destination, physical string) error {
	if err := os.Symlink(physical, destination); err == nil {
		return nil
	} else if !os.IsExist(err) {
		return err
	}
	existing, err := filepath.EvalSymlinks(destination)
	if err != nil {
		return fmt.Errorf("inspect existing projection path: %w", err)
	}
	if filepath.Clean(existing) != filepath.Clean(physical) {
		return fmt.Errorf("projection path already resolves to %q, want %q", existing, physical)
	}
	return nil
}

func ensurePathInsideDirectory(root, candidate string) error {
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve typed toolset directory %q: %w", root, err)
	}
	candidateResolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return fmt.Errorf("resolve toolset path %q: %w", candidate, err)
	}
	relative, err := filepath.Rel(rootResolved, candidateResolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("toolset path %q escapes typed directory %q", candidate, root)
	}
	return nil
}
