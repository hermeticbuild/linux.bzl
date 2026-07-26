package main

import "testing"

func TestRustcRelease(t *testing.T) {
	t.Parallel()
	got, err := rustcRelease("rustc 1.97.0 (placeholder 2026-01-01)\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.97.0" {
		t.Fatalf("rustcRelease() = %q, want 1.97.0", got)
	}
}

func TestRustcReleaseRejectsOtherTools(t *testing.T) {
	t.Parallel()
	if _, err := rustcRelease("cargo 1.97.0\n"); err == nil {
		t.Fatal("rustcRelease() unexpectedly accepted cargo output")
	}
}
