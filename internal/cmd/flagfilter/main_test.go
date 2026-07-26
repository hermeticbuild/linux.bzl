package main

import (
	"reflect"
	"testing"
)

func TestFilterFlagsRemovesExactTokens(t *testing.T) {
	got := filterFlags(
		[]string{"-O2", "-mgeneral-regs-only", "-Wno-psabi", "-mgeneral-regs-only"},
		[]string{"-mgeneral-regs-only"},
	)
	want := []string{"-O2", "-Wno-psabi"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterFlags() = %#v, want %#v", got, want)
	}
}

func TestResponseFile(t *testing.T) {
	got := responseFile([]string{"-O2", "-Wall"})
	if want := "-O2\n-Wall\n"; got != want {
		t.Fatalf("responseFile() = %q, want %q", got, want)
	}
}
