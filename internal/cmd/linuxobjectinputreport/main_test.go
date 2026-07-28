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
		"include/linux/kernel.h",
		"high-fanout non-header source inputs",
		"none",
		"largest actions by input count",
		"//:init_main",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report missing %q:\n%s", want, got)
		}
	}
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
