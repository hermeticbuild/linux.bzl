package kconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func observedHeadersTestCut(t *testing.T, sidecar bool) *ActionPlanFamilyExecutionCut {
	t.Helper()
	variants := []ActionPlanFamilyVariant{}
	for _, variant := range []struct{ name, other string }{{"base", "0"}, {"other", "1"}} {
		config := familyTestConfig("1", variant.other)
		plan := snapshotActionPlan(familyTestSnapshot(t, 1, "", config, ConfigDependencySet{Opaque: true, Reason: "cut is conservative"}))
		node := &plan.Nodes[0]
		node.Outputs[0].Path = "generated/value.inc"
		recipe := cloneActionRecipe(plan.Recipes[node.Recipe])
		if sidecar {
			node.Outputs = append(node.Outputs, ActionPlanOutput{Tree: "metadata", Path: "capture.state", ObservedPath: "some-side-effect"})
			recipe.Outputs = append(recipe.Outputs, "00000001")
			recipe.ObservedOutputs = map[string]string{"00000001": "some-side-effect"}
		}
		id, err := recipe.ID()
		if err != nil {
			t.Fatal(err)
		}
		node.Recipe = id
		plan.Recipes = map[string]ActionRecipe{id: recipe}
		if err := contentAddressActionPlanNodes(plan); err != nil {
			t.Fatal(err)
		}
		snapshot, err := canonicalActionPlanSnapshot(plan, map[string]ConfigDependencySet{
			plan.Nodes[0].ID: {Opaque: true, Reason: "cut is conservative"},
		}, config)
		if err != nil {
			t.Fatal(err)
		}
		variants = append(variants, ActionPlanFamilyVariant{Name: variant.name, Snapshot: snapshot})
	}
	family, err := BuildActionPlanFamily(variants)
	if err != nil {
		t.Fatal(err)
	}
	roots := []ActionPlanFamilyExecutionCutRoot{}
	for _, node := range family.Nodes {
		roots = append(roots, ActionPlanFamilyExecutionCutRoot{NodeID: node.ID, Slot: 0})
	}
	cut, err := NewActionPlanFamilyExecutionCut(family, roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 {
		t.Fatalf("want two different full-config producers, got %d", len(roots))
	}
	return cut
}

func observedHeadersTestStores(t *testing.T, cut *ActionPlanFamilyExecutionCut, content []byte) map[string]string {
	t.Helper()
	stores := map[string]string{}
	for _, output := range cut.Outputs() {
		directory := stores[output.Output.Tree]
		if directory == "" {
			directory = t.TempDir()
			stores[output.Output.Tree] = directory
		}
		filename := filepath.Join(directory, "nodes", output.NodeID, planOrdinal(output.Slot))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		value := content
		if output.Output.ObservedPath != "" {
			// The collector must verify this slot exists but must never interpret
			// its payload as a header or rewrite a writer. Envelope validation is
			// owned by the runner, not the ordinary-header collector.
			value = []byte("not C header bytes\x00")
		}
		if err := os.WriteFile(filename, value, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return stores
}

func observedHeadersTestOutputPath(stores map[string]string, output ActionPlanFamilyExecutionCutOutput) string {
	return filepath.Join(stores[output.Output.Tree], "nodes", output.NodeID, planOrdinal(output.Slot))
}

func TestFamilyObservedHeadersShareContentNotOwner(t *testing.T) {
	cut := observedHeadersTestCut(t, true)
	content := []byte("#define VALUE 7\n")
	stores := observedHeadersTestStores(t, cut, content)
	observed, err := cut.ObserveHeaders(stores)
	if err != nil {
		t.Fatal(err)
	}
	headers := observed.Headers()
	if len(headers) != 2 || headers[0].NodeID == headers[1].NodeID || headers[0].ContentID != headers[1].ContentID {
		t.Fatalf("equal bytes must share content without merging producer authority: %#v", headers)
	}
	if len(observed.Sources()) != 1 {
		t.Fatal("equal content was not deduplicated")
	}
	for _, header := range headers {
		if header.Output.Path != "generated/value.inc" || header.Output.ObservedPath != "" {
			t.Fatal(header)
		}
		got, ok := observed.Content(header.ContentID)
		if !ok || !bytes.Equal(got, content) {
			t.Fatalf("wrong actual bytes: %q", got)
		}
		got[0] = '!'
		if again, _ := observed.Content(header.ContentID); !bytes.Equal(again, content) {
			t.Fatal("content escaped mutably")
		}
	}
	headers[0].NodeID = "changed"
	if observed.Headers()[0].NodeID == "changed" {
		t.Fatal("header ownership escaped mutably")
	}
	first, err := observed.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	second, err := observed.CanonicalJSON()
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("manifest is not deterministic")
	}
	first[0] = '!'
	third, _ := observed.CanonicalJSON()
	if !bytes.Equal(second, third) {
		t.Fatal("manifest escaped mutably")
	}
}

func TestFamilyObservedHeadersContentAndModeAffectIdentity(t *testing.T) {
	for _, change := range []string{"bytes", "mode"} {
		t.Run(change, func(t *testing.T) {
			cut := observedHeadersTestCut(t, false)
			stores := observedHeadersTestStores(t, cut, []byte("#define VALUE 7\n"))
			filename := observedHeadersTestOutputPath(stores, cut.Outputs()[0])
			if change == "bytes" {
				if err := os.WriteFile(filename, []byte("#define VALUE 8\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(filename, 0o755); err != nil {
				t.Fatal(err)
			}
			observed, err := cut.ObserveHeaders(stores)
			if err != nil {
				t.Fatal(err)
			}
			if observed.Headers()[0].ContentID == observed.Headers()[1].ContentID || len(observed.Sources()) != 2 {
				t.Fatal("different bytes/mode shared an identity")
			}
		})
	}
}

func TestFamilyObservedHeadersRequireCompleteRegularOutputs(t *testing.T) {
	for _, failure := range []string{"missing-store", "missing-header", "missing-sidecar", "symlink-header", "symlink-parent", "directory-header"} {
		t.Run(failure, func(t *testing.T) {
			cut := observedHeadersTestCut(t, true)
			stores := observedHeadersTestStores(t, cut, []byte("header\n"))
			var ordinary, sidecar ActionPlanFamilyExecutionCutOutput
			for _, output := range cut.Outputs() {
				if output.Output.ObservedPath == "" {
					ordinary = output
				} else {
					sidecar = output
				}
			}
			filename := observedHeadersTestOutputPath(stores, ordinary)
			switch failure {
			case "missing-store":
				delete(stores, ordinary.Output.Tree)
			case "missing-header":
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
			case "missing-sidecar":
				if err := os.Remove(observedHeadersTestOutputPath(stores, sidecar)); err != nil {
					t.Fatal(err)
				}
			case "symlink-header":
				if err := os.Rename(filename, filename+".actual"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(filename)+".actual", filename); err != nil {
					t.Fatal(err)
				}
			case "symlink-parent":
				parent := filepath.Dir(filename)
				if err := os.Rename(parent, parent+".actual"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(parent)+".actual", parent); err != nil {
					t.Fatal(err)
				}
			case "directory-header":
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filename, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if observed, err := cut.ObserveHeaders(stores); err == nil || observed != nil {
				t.Fatalf("accepted incomplete/nonregular execution: %#v / %v", observed, err)
			}
		})
	}
}

func TestFamilyObservedHeadersBoundBytesBeforePublishing(t *testing.T) {
	cut := observedHeadersTestCut(t, false)
	stores := observedHeadersTestStores(t, cut, []byte("12345678"))
	for _, limits := range [][2]int{{7, 20}, {8, 15}, {0, 20}, {8, 0}} {
		if result, err := cut.observeHeaders(stores, limits[0], limits[1]); err == nil || result != nil {
			t.Fatalf("accepted limits %v: %#v / %v", limits, result, err)
		}
	}
	if _, err := cut.observeHeaders(stores, 8, 16); err != nil {
		t.Fatal(err)
	}
	if _, err := (*ActionPlanFamilyExecutionCut)(nil).ObserveHeaders(stores); err == nil {
		t.Fatal("accepted nil cut")
	}
	if _, err := (&ActionPlanFamilyExecutionCut{}).ObserveHeaders(stores); err == nil {
		t.Fatal("accepted unsealed cut")
	}
}

func TestFamilyObservedHeadersWriteExactContentTree(t *testing.T) {
	cut := observedHeadersTestCut(t, false)
	stores := observedHeadersTestStores(t, cut, []byte("#define VALUE 7\n"))
	if err := os.Chmod(observedHeadersTestOutputPath(stores, cut.Outputs()[0]), 0o755); err != nil {
		t.Fatal(err)
	}
	observed, err := cut.ObserveHeaders(stores)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := observed.WriteContentTree(directory); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantManifest, _ := observed.CanonicalJSON()
	if !bytes.Equal(manifest, wantManifest) {
		t.Fatal("wrong observation manifest")
	}
	for _, source := range observed.Sources() {
		if source.Namespace != LinuxKernelObservedHeaderSourceNamespace || source.ID != semanticFamilySourceID(source.Namespace, source.Path) {
			t.Fatal(source)
		}
		filename := filepath.Join(directory, filepath.FromSlash(source.Path))
		got, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filename)
		if err != nil {
			t.Fatal(err)
		}
		id := strings.TrimPrefix(source.Path, "content/")
		if observedHeaderContentID(got, uint32(info.Mode().Perm()&0o111)) != id {
			t.Fatal("CAS bytes/mode do not match identity")
		}
	}
	if err := observed.WriteContentTree(directory); err == nil {
		t.Fatal("overwrote an existing content tree")
	}
	names, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || !slices.Equal([]string{names[0].Name(), names[1].Name()}, []string{"content", "manifest.json"}) {
		t.Fatal("content tree has unexpected entries")
	}
}

func TestFamilyObservedHeadersEmptyCutAndReadPermissions(t *testing.T) {
	cut, initial, _ := familyReplayTest(t)
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{{Name: "base", Snapshot: initial}})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := NewActionPlanFamilyExecutionCut(family, nil)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := empty.ObserveHeaders(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.Headers()) != 0 || len(observed.Sources()) != 0 {
		t.Fatal("empty cut produced header observations")
	}
	if err := observed.WriteContentTree(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	stores := observedHeadersTestStores(t, cut, []byte("#define VALUE 7\n"))
	before, err := cut.ObserveHeaders(stores)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(observedHeadersTestOutputPath(stores, cut.Outputs()[0]), 0o444); err != nil {
		t.Fatal(err)
	}
	after, err := cut.ObserveHeaders(stores)
	if err != nil {
		t.Fatal(err)
	}
	if before.Headers()[0].ContentID != after.Headers()[0].ContentID {
		t.Fatal("read/write permissions unexpectedly salted executable-mode identity")
	}
}
