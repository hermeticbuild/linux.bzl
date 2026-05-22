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

func TestReportSharedInputsAndProducers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "include/linux/kernel.h", "header")
	writeFile(t, dir, "include/generated/autoconf.h", "generated")
	writeFile(t, dir, "init/main.c", "source")
	writeFile(t, dir, "kernel/fork.c", "source")

	aquery := filepath.Join(dir, "aquery.json")
	writeFile(t, dir, "aquery.json", `{
  "artifacts": [
    {"id": 1, "pathFragmentId": 3},
    {"id": 2, "pathFragmentId": 6},
    {"id": 3, "pathFragmentId": 8},
    {"id": 4, "pathFragmentId": 11},
    {"id": 5, "pathFragmentId": 15}
  ],
  "actions": [
    {"targetId": 1, "mnemonic": "LinuxGeneratedHeaders", "outputIds": [2]},
    {"targetId": 2, "mnemonic": "LinuxObjectCompile", "inputDepSetIds": [1], "outputIds": [4]},
    {"targetId": 3, "mnemonic": "LinuxObjectCompile", "inputDepSetIds": [2], "outputIds": [5]}
  ],
  "targets": [
    {"id": 1, "label": "//:generated_headers"},
    {"id": 2, "label": "//:init_main"},
    {"id": 3, "label": "//:kernel_fork"}
  ],
  "depSetOfFiles": [
    {"id": 1, "directArtifactIds": [1, 2, 3]},
    {"id": 2, "directArtifactIds": [1, 2, 4]}
  ],
  "pathFragments": [
    {"id": 1, "label": "include"},
    {"id": 2, "label": "linux", "parentId": 1},
    {"id": 3, "label": "kernel.h", "parentId": 2},
    {"id": 4, "label": "include"},
    {"id": 5, "label": "generated", "parentId": 4},
    {"id": 6, "label": "autoconf.h", "parentId": 5},
    {"id": 7, "label": "init"},
    {"id": 8, "label": "main.c", "parentId": 7},
    {"id": 9, "label": "main.o", "parentId": 8},
    {"id": 10, "label": "kernel"},
    {"id": 11, "label": "fork.c", "parentId": 10},
    {"id": 12, "label": "fork.o", "parentId": 11},
    {"id": 13, "label": "bazel-out"},
    {"id": 14, "label": "init_main.o", "parentId": 13},
    {"id": 15, "label": "kernel_fork.o", "parentId": 13}
  ]
}`)

	var out bytes.Buffer
	if err := run(&out, reportOptions{
		inputPath: aquery,
		mnemonic:  "LinuxObjectCompile",
		top:       10,
		sharedPct: 100,
		execroot:  dir,
	}); err != nil {
		t.Fatalf("run() failed: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"actions: 2",
		"input count min/p50/p95/max: 3 / 3 / 3 / 3",
		"include/linux/kernel.h",
		"include/generated/autoconf.h  [LinuxGeneratedHeaders //:generated_headers]",
		"LinuxGeneratedHeaders //:generated_headers",
		"high-fanout non-header source inputs",
		"none",
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
