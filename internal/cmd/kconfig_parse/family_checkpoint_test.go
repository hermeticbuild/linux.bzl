package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestLinuxFamilyCheckpointTransport(t *testing.T) {
	want := linuxFamilyCheckpoint{Schema: linuxFamilyCheckpointSchema, Variant: "base", Plan: json.RawMessage(`{"plan":1}`), Compiler: json.RawMessage(`{"compiler":2}`)}
	filename := filepath.Join(t.TempDir(), "checkpoints", "base.json.gz")
	if err := writeLinuxFamilyCheckpoint(filename, want); err != nil {
		t.Fatal(err)
	}
	got, err := readLinuxFamilyCheckpoint(filename)
	if err != nil || !reflect.DeepEqual(got, &want) {
		t.Fatalf("round trip = %#v, %v", got, err)
	}
	first, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(t.TempDir(), "base.json.gz")
	if err := writeLinuxFamilyCheckpoint(second, want); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(second)
	if err != nil || !bytes.Equal(first, data) {
		t.Fatalf("checkpoint transport is not deterministic: %v", err)
	}
	canonical, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	compress := func(data []byte) []byte {
		var output bytes.Buffer
		writer := gzip.NewWriter(&output)
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return output.Bytes()
	}
	for name, data := range map[string][]byte{
		"not gzip":        []byte("checkpoint"),
		"truncated":       first[:len(first)-1],
		"trailing bytes":  append(bytes.Clone(first), 'x'),
		"second member":   append(bytes.Clone(first), first...),
		"unknown field":   compress([]byte(strings.Replace(string(canonical), "{", `{"Unknown":1,`, 1))),
		"trailing JSON":   compress(append(bytes.Clone(canonical), []byte(`{}`)...)),
		"whitespace":      compress(append([]byte(" "), canonical...)),
		"wrong schema":    compress([]byte(strings.Replace(string(canonical), linuxFamilyCheckpointSchema, "old-schema", 1))),
		"duplicate field": compress([]byte(strings.Replace(string(canonical), "{", `{"Variant":"base",`, 1))),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.json.gz")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := readLinuxFamilyCheckpoint(path); err == nil {
				t.Fatal("accepted malformed checkpoint")
			}
		})
	}
	if _, err := readLinuxFamilyCheckpoint(t.TempDir()); err == nil {
		t.Fatal("accepted directory as checkpoint")
	}
	if err := writeLinuxFamilyCheckpoint(filepath.Join(t.TempDir(), "empty"), linuxFamilyCheckpoint{}); err == nil {
		t.Fatal("accepted empty checkpoint payloads")
	}
}

func TestLinuxCheckpointCompressedBudget(t *testing.T) {
	var output bytes.Buffer
	writer := checkpointLimitedWriter{output: &output, remaining: 3}
	if _, err := writer.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("d")); err == nil || output.String() != "abc" {
		t.Fatal("writer exceeded its budget")
	}
	filename := filepath.Join(t.TempDir(), "oversize")
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse file exercises the compressed bound without allocating the budget.
	if err := file.Truncate(kconfig.MaxActionPlanSnapshotCompressedBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readLinuxFamilyCheckpoint(filename); err == nil || !strings.Contains(err.Error(), "size/type") {
		t.Fatalf("oversized checkpoint = %v", err)
	}
}

func TestLinuxCheckpointSourceArtifactsRebindAliases(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	options := linuxKbuildProbeOptions{rootPath: root, sourceRoots: map[string]string{"nested": nested, "alias": alias, "external": external}}
	options.sourceRoots["virtual"] = "virtual"
	got, virtual, err := linuxCheckpointSourceArtifacts(options)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["alias"] != root || got["external"] != external {
		t.Fatalf("source artifacts did not collapse aliases: %#v", got)
	}
	if !reflect.DeepEqual(virtual, map[string]string{"virtual": "virtual"}) {
		t.Fatalf("virtual roots = %#v", virtual)
	}
	if options.sourceRoots["nested"] != nested || options.sourceRoots["alias"] != alias {
		t.Fatal("mutated caller source roots")
	}
	options.sourceRoots["missing"] = filepath.Join(root, "missing")
	if _, _, err := linuxCheckpointSourceArtifacts(options); err == nil {
		t.Fatal("accepted absent current source artifact")
	}
}
