package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// selectedRecipeFrontier binds a completed source traversal to one immutable
// recipe expansion. Its file view and owner resolver share the same persistent
// object-tree state; no later child or local writer can change either.
func selectedRecipeFrontier(
	frontier *kbuildRecursiveMakeFrontier,
	state kbuildFrontierState,
	directory string,
	immutableContents map[string]string,
	sourceOverlayDirectories []string,
) kconfig.KbuildControlRecipeFrontier {
	view := kbuildFrontierVirtualFileView{
		state: state, directory: directory,
		sourceOverlayDirectories: sourceOverlayDirectories,
		immutableContents:        immutableContents,
	}
	baselineNames := make([]string, 0, len(immutableContents))
	for name := range immutableContents {
		baselineNames = append(baselineNames, name)
	}
	sort.Strings(baselineNames)
	identity := sha256.New()
	for _, name := range baselineNames {
		identity.Write([]byte(name))
		identity.Write([]byte{0})
		identity.Write([]byte(immutableContents[name]))
		identity.Write([]byte{0})
	}
	visible := kbuildFrontierDigest(state)
	identity.Write(visible[:])
	identity.Write([]byte(kbuildRecursiveMakeFrontierID(frontier)))
	frontierID := hex.EncodeToString(identity.Sum(nil))
	selectedValue := func(path string) (*kbuildFrontierValue, error) {
		candidates := kbuildFrontierGlobalPathCandidates(path, view.directory, sourceOverlayDirectories)
		var selected *kbuildFrontierValue
		selectedPath := ""
		for _, candidate := range candidates {
			value, present := kbuildFrontierGet(state, candidate)
			if !present {
				continue
			}
			if selected != nil && (selectedPath != candidate || !sameFrontierValue(*selected, value)) {
				return nil, fmt.Errorf("recipe read %q has incomparable source-visible writers %q and %q", path, selectedPath, candidate)
			}
			selected = &value
			selectedPath = candidate
		}
		return selected, nil
	}
	return kconfig.KbuildControlRecipeFrontier{
		ID: frontierID, Files: view,
		ResolvePresenceArtifact: func(path string) (kconfig.KbuildControlReadArtifact, bool, error) {
			if !strings.HasPrefix(path, kbuildEvalObjectTree+"/") {
				return kconfig.KbuildControlReadArtifact{}, false, fmt.Errorf("recipe presence %q has no typed object-tree owner", path)
			}
			selected, err := selectedValue(path)
			if err != nil {
				return kconfig.KbuildControlReadArtifact{}, false, err
			}
			if selected == nil || selected.exact || selected.artifact.Path == "" {
				return kconfig.KbuildControlReadArtifact{}, false, nil
			}
			return kconfig.KbuildControlReadArtifact{
				Tree:     kconfig.CompactKbuildInvocationObjectTree,
				Identity: "presence:selection:" + selected.artifact.Profile + ":" + selected.artifact.Target + ":" + selected.artifact.Path,
				Version:  frontierID, Producer: selected.artifact,
			}, true, nil
		},
		ResolveArtifact: func(path string) (kconfig.KbuildControlReadArtifact, bool, error) {
			candidates := kbuildFrontierGlobalPathCandidates(path, view.directory, sourceOverlayDirectories)
			selected, err := selectedValue(path)
			if err != nil {
				return kconfig.KbuildControlReadArtifact{}, false, err
			}
			tree := kconfig.CompactKbuildInvocationObjectTree
			if path == kbuildEvalSourceTree || strings.HasPrefix(path, kbuildEvalSourceTree+"/") {
				tree = kconfig.CompactKbuildInvocationSourceTree
			}
			if selected != nil {
				if !selected.exact {
					return kconfig.KbuildControlReadArtifact{}, false, fmt.Errorf("recipe read %q has opaque selected writer %s:%s", path, selected.artifact.Profile, selected.artifact.Target)
				}
				version := sha256.Sum256([]byte(selected.content))
				return kconfig.KbuildControlReadArtifact{
					Tree: tree, Identity: "selection:" + selected.artifact.Profile + ":" + selected.artifact.Target + ":" + selected.artifact.Path,
					Version: hex.EncodeToString(version[:]), Producer: selected.artifact,
				}, true, nil
			}
			var baselinePath string
			var baseline string
			for _, candidate := range candidates {
				text, present := immutableContents[candidate]
				if !present {
					continue
				}
				if baselinePath != "" && (baselinePath != candidate || baseline != text) {
					return kconfig.KbuildControlReadArtifact{}, false, fmt.Errorf("recipe read %q has conflicting declared config projection aliases %q and %q", path, baselinePath, candidate)
				}
				baselinePath, baseline = candidate, text
			}
			if baselinePath == "" {
				return kconfig.KbuildControlReadArtifact{}, false, nil
			}
			version := sha256.Sum256([]byte(baseline))
			return kconfig.KbuildControlReadArtifact{
				Tree: tree, Identity: "config:" + baselinePath,
				Version: hex.EncodeToString(version[:]),
			}, true, nil
		},
	}
}
