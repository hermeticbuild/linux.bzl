package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func familyViewBatchFixture(t *testing.T) (string, string, string, []string) {
	t.Helper()
	dir := t.TempDir()
	plan, store, out := filepath.Join(dir, "plan"), filepath.Join(dir, "store"), filepath.Join(dir, "out")
	markers := []string{}
	for i, destination := range []string{"nested/first", "nested/second"} {
		node := strings.Repeat(string(rune('a'+i)), 64)
		marker := filepath.Join(plan, "variants", "base", "view", "metadata", "from", node, "00000000", "at", destination)
		source := filepath.Join(store, "nodes", node, "00000000")
		for _, filename := range []string{marker, source} {
			if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte(destination), 0o755); err != nil {
			t.Fatal(err)
		}
		markers = append(markers, marker)
	}
	return plan, store, out, markers
}

func TestFamilyViewBatchesShareOutputParentWithoutReadingOtherMarkers(t *testing.T) {
	plan, store, out, markers := familyViewBatchFixture(t)
	// An unselected malformed sibling proves this is an exact-input operation,
	// not a walk of all markers which happens to have a smaller expected count.
	if err := os.WriteFile(filepath.Join(plan, "variants", "base", "view", "metadata", "unselected"), []byte("not a marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, marker := range markers {
		if err := projectFamilyView(plan, "base", "metadata", store, out, 1, true, []string{marker}); err != nil {
			t.Fatal(err)
		}
	}
	for _, destination := range []string{"nested/first", "nested/second"} {
		got, err := os.ReadFile(filepath.Join(out, destination))
		if err != nil || string(got) != destination {
			t.Fatalf("%s = %q, %v", destination, got, err)
		}
	}
	if err := projectFamilyView(plan, "base", "metadata", store, out, 1, true, markers[:1]); err == nil || !strings.Contains(err.Error(), "pre-existing leaf") {
		t.Fatalf("repeated batch error = %v, want stale selected leaf rejection", err)
	}
}

func TestFamilyViewBatchRejectsDuplicateAndForeignMarkers(t *testing.T) {
	for _, kind := range []string{"duplicate", "foreign", "marker symlink", "source symlink"} {
		t.Run(kind, func(t *testing.T) {
			plan, store, out, markers := familyViewBatchFixture(t)
			selected := markers[:1]
			switch kind {
			case "duplicate":
				selected = []string{markers[0], markers[0]}
			case "foreign":
				plan = filepath.Join(plan, "another-plan")
			case "marker symlink":
				if err := os.Remove(markers[0]); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(markers[1], markers[0]); err != nil {
					t.Fatal(err)
				}
			case "source symlink":
				source := filepath.Join(store, "nodes", strings.Repeat("a", 64), "00000000")
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(markers[1], source); err != nil {
					t.Fatal(err)
				}
			}
			if err := projectFamilyView(plan, "base", "metadata", store, out, len(selected), true, selected); err == nil {
				t.Fatal("accepted invalid batch")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("invalid inputs created output root: %v", err)
			}
		})
	}
}

func TestFamilyViewBatchRejectsSelectedSymlinkAncestorsAndLeaves(t *testing.T) {
	for _, kind := range []string{"root", "ancestor", "leaf"} {
		t.Run(kind, func(t *testing.T) {
			plan, store, out, markers := familyViewBatchFixture(t)
			target := t.TempDir()
			link := out
			if kind == "ancestor" {
				link = filepath.Join(out, "nested")
			}
			if kind == "leaf" {
				link = filepath.Join(out, "nested", "first")
			}
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if err := projectFamilyView(plan, "base", "metadata", store, out, 1, true, markers[:1]); err == nil {
				t.Fatal("accepted symlink output")
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatalf("wrote outside declared output: %v, %v", entries, err)
			}
		})
	}
}
