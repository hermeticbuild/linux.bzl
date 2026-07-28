// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportSharedInputs(t *testing.T) {
	dir := t.TempDir()

	aquery := filepath.Join(dir, "aquery.json")
	writeFile(t, dir, "aquery.json", `{
	  "artifacts": [
	    {"id": 1, "pathFragmentId": 3},
	    {"id": 2, "pathFragmentId": 5},
	    {"id": 3, "pathFragmentId": 7}
	  ],
	  "actions": [
	    {"targetId": 1, "mnemonic": "LinuxObjectCompile", "inputDepSetIds": [1]},
	    {"targetId": 2, "mnemonic": "LinuxObjectCompile", "inputDepSetIds": [2]}
	  ],
	  "targets": [
	    {"id": 1, "label": "//:init_main"},
	    {"id": 2, "label": "//:kernel_fork"}
	  ],
	  "depSetOfFiles": [
	    {"id": 1, "directArtifactIds": [1, 2]},
	    {"id": 2, "directArtifactIds": [1, 3]}
	  ],
	  "pathFragments": [
	    {"id": 1, "label": "include"},
	    {"id": 2, "label": "linux", "parentId": 1},
	    {"id": 3, "label": "kernel.h", "parentId": 2},
	    {"id": 4, "label": "init"},
	    {"id": 5, "label": "main.c", "parentId": 4},
	    {"id": 6, "label": "kernel"},
	    {"id": 7, "label": "fork.c", "parentId": 6}
	  ]
	}`)

	var out bytes.Buffer
	if err := run(&out, reportOptions{
		inputPath: aquery,
		mnemonic:  "LinuxObjectCompile",
		top:       10,
		sharedPct: 100,
	}); err != nil {
		t.Fatalf("run() failed: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"actions: 2",
		"input count min/p50/p95/max: 2 / 2 / 2 / 2",
		"materialized input bytes: disabled (pass -execroot)",
		"include/linux/kernel.h",
		"high-fanout non-header source inputs",
		"none",
		"producer fanout (unique LinuxObjectCompile consumer actions; producers present in aquery)",
		"producer-resolved input artifacts: 0 / 3 unique inputs",
		"hint: query deps(<target>)",
		"largest actions by input count",
		"//:init_main",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report missing %q:\n%s", want, got)
		}
	}
}

func TestProducerFanoutCountsUniqueConsumers(t *testing.T) {
	aq := &aqueryOutput{
		Artifacts: []artifact{
			{ID: 1, PathFragmentID: 2},
			{ID: 2, PathFragmentID: 3},
		},
		Actions: []action{
			{
				TargetID:        1,
				Mnemonic:        "LinuxGeneratedHeaders",
				OutputIDs:       []int{1, 2},
				PrimaryOutputID: 1,
			},
			{
				TargetID:       2,
				Mnemonic:       "LinuxObjectCompile",
				InputDepSetIDs: []int{1},
			},
			{
				TargetID:       3,
				Mnemonic:       "LinuxObjectCompile",
				InputDepSetIDs: []int{2},
			},
		},
		Targets: []target{
			{ID: 1, Label: "//:generated_headers"},
			{ID: 2, Label: "//:first_consumer"},
			{ID: 3, Label: "//:second_consumer"},
		},
		DepSetOfFiles: []depSet{
			{
				ID:                  1,
				DirectArtifactIDs:   []int{1, 2},
				TransitiveDepSetIDs: []int{3},
			},
			{ID: 2, DirectArtifactIDs: []int{2}},
			{ID: 3, DirectArtifactIDs: []int{1}},
		},
		PathFragments: []pathFragment{
			{ID: 1, Label: "bazel-out"},
			{ID: 2, Label: "generated_headers", ParentID: 1},
			{ID: 3, Label: "generated_headers.params", ParentID: 1},
		},
	}

	got := renderReport(t, aq)
	want := "2  LinuxGeneratedHeaders //:generated_headers [primary: bazel-out/generated_headers; outputs: 2]"
	if !strings.Contains(got, want) {
		t.Fatalf("producer fanout missing %q:\n%s", want, got)
	}
	if count := strings.Count(got, "LinuxGeneratedHeaders //:generated_headers"); count != 1 {
		t.Fatalf("producer emitted %d rows, want 1:\n%s", count, got)
	}
	if !strings.Contains(got, "producer-resolved input artifacts: 2 / 2 unique inputs") {
		t.Fatalf("producer coverage is incorrect:\n%s", got)
	}
}

func TestProducerFanoutKeepsDistinctActionsWithSameOwner(t *testing.T) {
	aq := &aqueryOutput{
		Artifacts: []artifact{
			{ID: 1, PathFragmentID: 2},
			{ID: 2, PathFragmentID: 3},
		},
		Actions: []action{
			{
				TargetID:        1,
				ConfigurationID: 2,
				Mnemonic:        "Generate",
				OutputIDs:       []int{2},
				PrimaryOutputID: 2,
			},
			{
				TargetID:        1,
				ConfigurationID: 1,
				Mnemonic:        "Generate",
				OutputIDs:       []int{1},
				PrimaryOutputID: 1,
			},
			{
				TargetID:       2,
				Mnemonic:       "LinuxObjectCompile",
				InputDepSetIDs: []int{1},
			},
		},
		Targets: []target{
			{ID: 1, Label: "//:shared_owner"},
			{ID: 2, Label: "//:consumer"},
		},
		DepSetOfFiles: []depSet{
			{ID: 1, DirectArtifactIDs: []int{1, 2}},
		},
		PathFragments: []pathFragment{
			{ID: 1, Label: "bazel-out"},
			{ID: 2, Label: "a.out", ParentID: 1},
			{ID: 3, Label: "b.out", ParentID: 1},
		},
	}

	got := renderReport(t, aq)
	if count := strings.Count(got, "Generate //:shared_owner"); count != 2 {
		t.Fatalf("same-owner producer actions emitted %d rows, want 2:\n%s", count, got)
	}
	first := strings.Index(got, "Generate //:shared_owner [primary: bazel-out/a.out; outputs: 1]")
	second := strings.Index(got, "Generate //:shared_owner [primary: bazel-out/b.out; outputs: 1]")
	if first < 0 || second < 0 {
		t.Fatalf("producer rows are missing:\n%s", got)
	}
	if first >= second {
		t.Fatalf("producer rows are not deterministically sorted by primary output:\n%s", got)
	}
}

func TestDuplicateProducerOwnershipFails(t *testing.T) {
	_, err := newModel(&aqueryOutput{
		Actions: []action{
			{Mnemonic: "FirstProducer", OutputIDs: []int{1}},
			{Mnemonic: "SecondProducer", OutputIDs: []int{1}},
		},
	})
	if err == nil {
		t.Fatal("newModel() succeeded with duplicate output ownership")
	}
	if !strings.Contains(err.Error(), "artifact 1 is output by multiple actions (0 and 1)") {
		t.Fatalf("newModel() error = %q", err)
	}
}

func TestMaterializedBytesDeduplicateAliases(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "data", "12345")
	writeFile(t, dir, "empty", "")
	writeFile(t, dir, "copy", "12345")
	if err := os.Link(filepath.Join(dir, "data"), filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data", filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}

	got := renderReportWithOptions(t, inputAquery(
		inputArtifact{path: "data"},
		inputArtifact{path: "empty"},
		inputArtifact{path: "hardlink"},
		inputArtifact{path: "symlink"},
		inputArtifact{path: "copy"},
	), reportOptions{
		mnemonic:  "LinuxObjectCompile",
		top:       10,
		sharedPct: 100,
		execroot:  dir,
	})

	for _, want := range []string{
		"byte coverage: 1 / 1 actions complete; 0 distinct inputs unavailable (0 action uses)",
		"materialized input bytes min/p50/p95/max (1 complete actions): 10 B / 10 B / 10 B / 10 B",
		"largest complete actions by materialized input bytes:",
		"10 B     5 inputs  //:consumer",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report missing %q:\n%s", want, got)
		}
	}
}

func TestMaterializedBytesReportMissingInputs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "present", "data")
	aq := inputAquery(
		inputArtifact{path: "present"},
		inputArtifact{path: "generated-missing"},
		inputArtifact{path: "source-missing"},
	)
	aq.Actions = append([]action{{
		TargetID:        2,
		Mnemonic:        "Generate",
		OutputIDs:       []int{2},
		PrimaryOutputID: 2,
	}}, aq.Actions...)
	aq.Targets = append(aq.Targets, target{ID: 2, Label: "//:generator"})

	got := renderReportWithOptions(t, aq, reportOptions{
		mnemonic:  "LinuxObjectCompile",
		top:       10,
		sharedPct: 100,
		execroot:  dir,
	})

	for _, want := range []string{
		"byte coverage: 0 / 1 actions complete; 2 distinct inputs unavailable (2 action uses)",
		"byte issues: not-found=1 unmaterialized=1",
		"materialized input bytes: unavailable (no complete actions)",
		"largest complete actions by materialized input bytes:\n  none",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report missing %q:\n%s", want, got)
		}
	}
}

func TestMaterializedByteStatsUseCompleteActions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "present", "abc")
	aq := inputAquery(
		inputArtifact{path: "present"},
		inputArtifact{path: "missing"},
	)
	aq.Actions = []action{
		{TargetID: 1, Mnemonic: "LinuxObjectCompile", InputDepSetIDs: []int{1}},
		{TargetID: 2, Mnemonic: "LinuxObjectCompile", InputDepSetIDs: []int{2}},
	}
	aq.Targets = []target{
		{ID: 1, Label: "//:complete"},
		{ID: 2, Label: "//:incomplete"},
	}
	aq.DepSetOfFiles = []depSet{
		{ID: 1, DirectArtifactIDs: []int{1}},
		{ID: 2, DirectArtifactIDs: []int{1, 2}},
	}

	got := renderReportWithOptions(t, aq, reportOptions{
		mnemonic:  "LinuxObjectCompile",
		top:       10,
		sharedPct: 100,
		execroot:  dir,
	})

	for _, want := range []string{
		"byte coverage: 1 / 2 actions complete; 1 distinct inputs unavailable (1 action uses)",
		"materialized input bytes min/p50/p95/max (1 complete actions): 3 B / 3 B / 3 B / 3 B",
		"3 B     1 inputs  //:complete",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report missing %q:\n%s", want, got)
		}
	}
	byteSectionIndex := strings.Index(got, "largest complete actions by materialized input bytes:")
	if byteSectionIndex < 0 {
		t.Fatalf("byte ranking is missing:\n%s", got)
	}
	byteSection := got[byteSectionIndex:]
	if strings.Contains(byteSection, "//:incomplete") {
		t.Fatalf("incomplete action appeared in byte ranking:\n%s", got)
	}
}

func TestMaterializedBytesScanTreeAndDirectoryInputs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "tree/first", "abc")
	writeFile(t, dir, "tree/nested/second", "de")
	if err := os.Mkdir(filepath.Join(dir, "empty-tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "directory/value", "wxyz")

	got := renderReportWithOptions(t, inputAquery(
		inputArtifact{path: "tree", tree: true},
		inputArtifact{path: "empty-tree", tree: true},
		inputArtifact{path: "directory"},
	), reportOptions{
		mnemonic:  "LinuxObjectCompile",
		top:       10,
		sharedPct: 100,
		execroot:  dir,
	})

	for _, want := range []string{
		"byte coverage: 1 / 1 actions complete; 0 distinct inputs unavailable (0 action uses)",
		"materialized input bytes min/p50/p95/max (1 complete actions): 9 B / 9 B / 9 B / 9 B",
		"9 B     3 inputs  //:consumer",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report missing %q:\n%s", want, got)
		}
	}
}

func TestMaterializedBytesReportPartialTree(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "tree/known", "data")
	if err := os.Symlink("missing", filepath.Join(dir, "tree/broken-first")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("also-missing", filepath.Join(dir, "tree/broken-second")); err != nil {
		t.Fatal(err)
	}

	got := renderReportWithOptions(t, inputAquery(
		inputArtifact{path: "tree", tree: true},
	), reportOptions{
		mnemonic:  "LinuxObjectCompile",
		top:       10,
		sharedPct: 100,
		execroot:  dir,
	})

	for _, want := range []string{
		"byte coverage: 0 / 1 actions complete; 1 distinct inputs unavailable (1 action uses)",
		"byte issues: broken-symlink=2",
		">=4 B  tree",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report missing %q:\n%s", want, got)
		}
	}
}

func TestMaterializedBytesReportSymlinkCycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("second", filepath.Join(dir, "first")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("first", filepath.Join(dir, "second")); err != nil {
		t.Fatal(err)
	}

	got := renderReportWithOptions(t, inputAquery(
		inputArtifact{path: "first"},
	), reportOptions{
		mnemonic:  "LinuxObjectCompile",
		top:       10,
		sharedPct: 100,
		execroot:  dir,
	})

	if !strings.Contains(got, "byte issues: symlink-cycle=1") {
		t.Fatalf("report does not classify the symlink cycle:\n%s", got)
	}
}

func TestExecrootValidation(t *testing.T) {
	aq := inputAquery(inputArtifact{path: "input"})
	for name, execroot := range map[string]string{
		"missing": filepath.Join(t.TempDir(), "missing"),
		"file":    writeTempFile(t, "not-a-directory"),
	} {
		t.Run(name, func(t *testing.T) {
			model, err := newModel(aq)
			if err != nil {
				t.Fatalf("newModel() failed: %v", err)
			}
			err = writeReport(&bytes.Buffer{}, model, reportOptions{
				mnemonic:  "LinuxObjectCompile",
				top:       10,
				sharedPct: 100,
				execroot:  execroot,
			})
			if err == nil || !strings.Contains(err.Error(), "-execroot") {
				t.Fatalf("writeReport() error = %v, want -execroot validation failure", err)
			}
		})
	}
}

type inputArtifact struct {
	path string
	tree bool
}

func inputAquery(inputs ...inputArtifact) *aqueryOutput {
	aq := &aqueryOutput{
		Actions: []action{{
			TargetID:       1,
			Mnemonic:       "LinuxObjectCompile",
			InputDepSetIDs: []int{1},
		}},
		Targets:       []target{{ID: 1, Label: "//:consumer"}},
		DepSetOfFiles: []depSet{{ID: 1}},
	}
	for index, input := range inputs {
		id := index + 1
		aq.Artifacts = append(aq.Artifacts, artifact{
			ID:             id,
			PathFragmentID: id,
			IsTreeArtifact: input.tree,
		})
		aq.DepSetOfFiles[0].DirectArtifactIDs = append(aq.DepSetOfFiles[0].DirectArtifactIDs, id)
		aq.PathFragments = append(aq.PathFragments, pathFragment{
			ID:    id,
			Label: input.path,
		})
	}
	return aq
}

func renderReport(t *testing.T, aq *aqueryOutput) string {
	t.Helper()
	return renderReportWithOptions(t, aq, reportOptions{
		mnemonic:  "LinuxObjectCompile",
		top:       10,
		sharedPct: 100,
	})
}

func renderReportWithOptions(t *testing.T, aq *aqueryOutput, opts reportOptions) string {
	t.Helper()
	model, err := newModel(aq)
	if err != nil {
		t.Fatalf("newModel() failed: %v", err)
	}
	var out bytes.Buffer
	if err := writeReport(&out, model, opts); err != nil {
		t.Fatalf("writeReport() failed: %v", err)
	}
	return out.String()
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
