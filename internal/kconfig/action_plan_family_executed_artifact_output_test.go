package kconfig

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func TestFamilyArtifactContentTree(t *testing.T) {
	for _, change := range []string{"", "existing output", "missing content", "changed bytes", "changed mode", "changed cut", "lost slot", "changed slot"} {
		t.Run(change, func(t *testing.T) {
			variant := executedArtifactVariantForTest(t, "0")
			family, err := BuildConservativeActionPlanFamily([]ActionPlanFamilyVariant{variant})
			if err != nil {
				t.Fatal(err)
			}
			var roots []ActionPlanFamilyExecutionCutRoot
			for _, node := range family.Nodes {
				if node.Stage == "prehost" {
					roots = append(roots, ActionPlanFamilyExecutionCutRoot{NodeID: node.ID, Slot: 0})
				}
			}
			cut, err := NewActionPlanFamilyExecutionCut(family, roots)
			if err != nil {
				t.Fatal(err)
			}
			observed, err := observeExecutedArtifacts(cut, executedArtifactStoresForTest(t, cut, toolaction.ObservedOutputAbsent), 1024, 16384)
			if err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			sentinel := filepath.Join(directory, "existing")
			var contentID string
			for id := range observed.contents {
				contentID = id
				break
			}
			if contentID == "" {
				t.Fatal("fixture has no contents")
			}
			switch change {
			case "existing output":
				if err := os.WriteFile(sentinel, []byte("preserve me"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "missing content":
				delete(observed.contents, contentID)
			case "changed bytes":
				observed.contents[contentID] = []byte("forged")
			case "changed mode":
				observed.modes[contentID] ^= 0o111
			case "changed cut":
				observed.cutID = "forged"
			case "lost slot":
				for key := range observed.outputs {
					delete(observed.outputs, key)
					break
				}
			case "changed slot":
				observed.outputs = maps.Clone(observed.outputs)
				for key, output := range observed.outputs {
					output.output.Slot++
					observed.outputs[key] = output
					break
				}
			}
			err = writeExecutedArtifactContentTree(observed, directory)
			if change != "" {
				if err == nil {
					t.Fatalf("accepted %s", change)
				}
				if _, err := os.Stat(filepath.Join(directory, "manifest.json")); !os.IsNotExist(err) {
					t.Fatal("published manifest on error")
				}
				if change == "existing output" {
					data, err := os.ReadFile(sentinel)
					if err != nil || string(data) != "preserve me" {
						t.Fatal("overwrote existing output")
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for id, contents := range observed.contents {
				file := filepath.Join(directory, "content", id)
				actual, err := os.ReadFile(file)
				if err != nil || !bytes.Equal(contents, actual) {
					t.Fatal("published different bytes")
				}
				info, err := os.Stat(file)
				if err != nil || uint32(info.Mode().Perm()&0o111) != observed.modes[id] {
					t.Fatal("lost executable mode")
				}
			}
			if _, err := os.Stat(filepath.Join(directory, "manifest.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
