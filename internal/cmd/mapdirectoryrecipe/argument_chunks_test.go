package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestArgumentChunksRoundTrip(t *testing.T) {
	want := []string{"", "@literal", "-argument_chunks", "with spaces\nand\ttabs\r", "a=b=c", "repeated", "repeated"}
	args := []string{"-argument_chunks"}
	for index, chunk := range [][]string{want[:3], {}, want[3:]} {
		filename := filepath.Join(t.TempDir(), "chunk.json")
		if err := writeArgumentChunk(filename, chunk); err != nil {
			t.Fatalf("chunk %d: %v", index, err)
		}
		args = append(args, filename)
	}
	got, err := expandArgumentChunks(args)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("expanded = %q, %v; want %q", got, err, want)
	}
}

func TestArgumentChunksRejectMalformedFiles(t *testing.T) {
	for _, data := range []string{"null", "{}", "[1]", "[null]", "[\"\\ud800\"]", "[\"x\"] []", "[\"\\u0000\"]", strings.Repeat(" ", maxArgumentChunkBytes+1)} {
		filename := filepath.Join(t.TempDir(), "invalid.json")
		if err := os.WriteFile(filename, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := expandArgumentChunks([]string{"-argument_chunks", filename}); err == nil {
			t.Fatalf("accepted malformed chunk of %d bytes", len(data))
		}
	}
	for _, args := range [][]string{{"-argument_chunks"}, {"-argument_chunks", t.TempDir()}, {"-argument_chunks", filepath.Join(t.TempDir(), "missing")}} {
		if _, err := expandArgumentChunks(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestArgumentChunksBounds(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "chunk.json")
	chunk := make([]string, maxParameterFileArguments)
	if err := writeArgumentChunk(filename, chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := expandArgumentChunks([]string{"-argument_chunks", filename}); err != nil {
		t.Fatal(err)
	}
	if _, err := expandArgumentChunks([]string{"-argument_chunks", filename, filename}); err == nil {
		t.Fatal("accepted too many arguments across chunks")
	}
	for _, args := range [][]string{append(chunk, ""), {"nul\x00"}, {"invalid\xff"}, {strings.Repeat("x", maxArgumentChunkBytes)}} {
		if err := writeArgumentChunk(filename, args); err == nil {
			t.Fatal("accepted oversized/invalid writer input")
		}
	}
	if err := writeArgumentChunk(filename, []string{strings.Repeat("x", maxArgumentChunkBytes-4)}); err != nil {
		t.Fatal(err)
	}
	args := []string{"-argument_chunks"}
	for range 65 {
		args = append(args, filename)
	}
	if _, err := expandArgumentChunks(args); err == nil {
		t.Fatal("accepted too many total bytes")
	}
}
