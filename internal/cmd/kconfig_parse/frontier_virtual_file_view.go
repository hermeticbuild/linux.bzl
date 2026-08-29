package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// kbuildFrontierArtifactView exposes the same persistent state to compact graph
// selection without serializing or copying its cumulative artifact slice.
type kbuildFrontierArtifactView struct {
	state kbuildFrontierState
}

func (view kbuildFrontierArtifactView) Len() int {
	return kbuildFrontierLen(view.state)
}

func (view kbuildFrontierArtifactView) Get(path string) (kconfig.CompactKbuildVisibleArtifact, bool) {
	value, ok := kbuildFrontierGet(view.state, path)
	if !ok {
		return kconfig.CompactKbuildVisibleArtifact{}, false
	}
	return value.artifact, true
}

func (view kbuildFrontierArtifactView) Range(
	prefix string,
	visit func(kconfig.CompactKbuildVisibleArtifact) bool,
) {
	kbuildFrontierRangePrefix(view.state, prefix, func(_ string, value kbuildFrontierValue) bool {
		return visit(value.artifact)
	})
}

// kbuildFrontierVirtualFileView presents one immutable recursive-Make object
// frontier directly to the Kbuild evaluator. It derives Make-visible aliases
// only for queried prefixes instead of materializing two aliases and an exact
// content map for every inherited artifact at every child invocation.
type kbuildFrontierVirtualFileView struct {
	state                    kbuildFrontierState
	directory                string
	sourceOverlayDirectories []string
}

func (view kbuildFrontierVirtualFileView) Match(pattern string) []string {
	pattern = filepath.ToSlash(pattern)
	prefixes := kbuildFrontierPatternPrefixes(pattern, view.directory, view.sourceOverlayDirectories)
	if len(prefixes) == 0 {
		return nil
	}
	visited := map[string]bool{}
	matches := map[string]bool{}
	for _, prefix := range prefixes {
		kbuildFrontierRangePrefix(view.state, prefix, func(path string, _ kbuildFrontierValue) bool {
			if visited[path] {
				return true
			}
			visited[path] = true
			first, second := kbuildInvocationVirtualPathAliasPair(path, view.directory)
			aliases := []string{first, second}
			if kbuildFrontierPathWithinSourceOverlay(path, view.sourceOverlayDirectories) {
				aliases = append(aliases, kbuildEvalSourceTree+"/"+path)
			}
			for _, alias := range aliases {
				if alias == "" {
					continue
				}
				if matched, err := filepath.Match(pattern, alias); err == nil && matched {
					matches[alias] = true
				}
			}
			return true
		})
	}
	result := make([]string, 0, len(matches))
	for match := range matches {
		result = append(result, match)
	}
	sort.Strings(result)
	return result
}

func (view kbuildFrontierVirtualFileView) Read(path string) (string, bool, bool, error) {
	paths := kbuildFrontierGlobalPathCandidates(path, view.directory, view.sourceOverlayDirectories)
	exists := false
	exact := false
	content := ""
	exactPath := ""
	for _, global := range paths {
		value, found := kbuildFrontierGet(view.state, global)
		if !found {
			continue
		}
		exists = true
		if !value.exact {
			continue
		}
		if exact && content != value.content {
			return "", false, false, fmt.Errorf(
				"virtual path alias %q has conflicting exact contents from %q and %q",
				path, exactPath, global,
			)
		}
		exact = true
		content = value.content
		exactPath = global
	}
	return content, exists, exact, nil
}

// kbuildFrontierPatternPrefixes inverts both aliases exposed by
// kbuildInvocationVirtualPathAliasPair. A first-component wildcard can match
// either ".." in a cwd-relative alias or the object-tree marker, so that case
// deliberately falls back to a full frontier range before final matching.
func kbuildFrontierPatternPrefixes(pattern, directory string, sourceOverlayDirectories []string) []string {
	prefixes := []string{}
	add := func(globalPattern string, ok bool) {
		if !ok {
			return
		}
		prefix := kbuildPatternLiteralPrefix(globalPattern)
		if !slicesContainsString(prefixes, prefix) {
			prefixes = append(prefixes, prefix)
		}
	}

	if slash := strings.IndexByte(pattern, '/'); slash >= 0 {
		first := pattern[:slash]
		if first == kbuildEvalObjectTree {
			add(kbuildCanonicalFrontierPattern(pattern[slash+1:]))
			return prefixes
		}
		if first == kbuildEvalSourceTree {
			if len(sourceOverlayDirectories) != 0 {
				add(kbuildCanonicalFrontierPattern(pattern[slash+1:]))
			}
			return prefixes
		}
		if matched, err := filepath.Match(first, kbuildEvalObjectTree); err == nil && matched {
			add(kbuildCanonicalFrontierPattern(pattern[slash+1:]))
		}
		if matched, err := filepath.Match(first, kbuildEvalSourceTree); err == nil && matched && len(sourceOverlayDirectories) != 0 {
			add(kbuildCanonicalFrontierPattern(pattern[slash+1:]))
		}
	}

	if !filepath.IsAbs(pattern) {
		first := pattern
		if slash := strings.IndexByte(first, '/'); slash >= 0 {
			first = first[:slash]
			if strings.ContainsAny(first, "*?[\\") {
				add("", true)
				return prefixes
			}
		}
		joined := filepath.ToSlash(filepath.Join(
			filepath.FromSlash(directory), filepath.FromSlash(pattern),
		))
		add(kbuildCanonicalFrontierPattern(joined))
	}
	return prefixes
}

func kbuildCanonicalFrontierPattern(pattern string) (string, bool) {
	pattern = filepath.ToSlash(pattern)
	pattern = strings.TrimPrefix(pattern, "./")
	if pattern == "" || pattern == "." || pattern == ".." || strings.HasPrefix(pattern, "../") {
		return "", false
	}
	return pattern, true
}

func kbuildFrontierGlobalPathCandidates(path, directory string, sourceOverlayDirectories []string) []string {
	path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	candidates := []string{}
	add := func(global string) {
		global = filepath.ToSlash(filepath.Clean(filepath.FromSlash(global)))
		global = strings.TrimPrefix(global, "./")
		if global == "" || global == "." || global == ".." || strings.HasPrefix(global, "../") ||
			slicesContainsString(candidates, global) {
			return
		}
		candidates = append(candidates, global)
	}
	if path == kbuildEvalObjectTree || path == kbuildEvalSourceTree {
		return candidates
	}
	if strings.HasPrefix(path, kbuildEvalObjectTree+"/") {
		add(strings.TrimPrefix(path, kbuildEvalObjectTree+"/"))
		return candidates
	}
	if strings.HasPrefix(path, kbuildEvalSourceTree+"/") {
		candidate := strings.TrimPrefix(path, kbuildEvalSourceTree+"/")
		if kbuildFrontierPathWithinSourceOverlay(candidate, sourceOverlayDirectories) {
			add(candidate)
		}
		return candidates
	}
	if !filepath.IsAbs(path) {
		add(filepath.ToSlash(filepath.Join(filepath.FromSlash(directory), filepath.FromSlash(path))))
	}
	return candidates
}

func kbuildFrontierPathWithinSourceOverlay(path string, sourceOverlayDirectories []string) bool {
	path = kconfig.CanonicalKbuildGraphTarget(path)
	for _, directory := range sourceOverlayDirectories {
		if path == directory || strings.HasPrefix(path, directory+"/") {
			return true
		}
	}
	return false
}

// kbuildFrontierSourceOverlayDirectories returns only nested declared source
// roots. Those roots are merged with their same-path writable object subtree
// for external-module execution. The broad kernel source root is deliberately
// excluded so an ordinary $(srctree) read can never observe generated files.
func kbuildFrontierSourceOverlayDirectories(sourceRoots map[string]string) []string {
	directories := map[string]bool{}
	prefix := kbuildEvalSourceTree + "/"
	for root := range sourceRoots {
		root = filepath.ToSlash(filepath.Clean(filepath.FromSlash(root)))
		if !strings.HasPrefix(root, prefix) {
			continue
		}
		directory := kconfig.CanonicalKbuildGraphTarget(strings.TrimPrefix(root, prefix))
		if directory != "" && directory != "." {
			directories[directory] = true
		}
	}
	result := make([]string, 0, len(directories))
	for directory := range directories {
		result = append(result, directory)
	}
	sort.Strings(result)
	return result
}

func slicesContainsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func kbuildPatternLiteralPrefix(pattern string) string {
	if index := strings.IndexAny(pattern, "*?[\\"); index >= 0 {
		return pattern[:index]
	}
	return pattern
}
