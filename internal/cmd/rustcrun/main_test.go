package main

import (
	"reflect"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/rusttoolchain"
)

func TestApplyPredicates(t *testing.T) {
	probe := rusttoolchain.Probe{
		Schema:          rusttoolchain.ProbeSchema,
		VersionText:     "rustc 1.98.0-nightly (012345678 2026-06-24)",
		Release:         "1.98.0-nightly",
		Semver:          "1.98.0",
		Channel:         "nightly",
		CommitHash:      "0123456789",
		LLVMVersion:     "22.1.7",
		VersionCode:     109800,
		LLVMVersionCode: 220107,
	}
	got, err := applyPredicates(probe, []string{"rustc", "--old", "--keep"}, []versionPredicate{{
		AtLeast:    "1.98.0",
		Add:        []string{"--new"},
		Remove:     []string{"--old"},
		ElseAdd:    []string{"--fallback"},
		ElseRemove: []string{"--keep"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rustc", "--keep", "--new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestApplyPredicatesElse(t *testing.T) {
	probe := rusttoolchain.Probe{
		Schema:          rusttoolchain.ProbeSchema,
		VersionText:     "rustc 1.78.0 (9b00956e5 2024-04-29)",
		Release:         "1.78.0",
		Semver:          "1.78.0",
		Channel:         "stable",
		CommitHash:      "9b00956e56009bab2aa15d7bff10916599e3d6d6",
		LLVMVersion:     "18.1.2",
		VersionCode:     107800,
		LLVMVersionCode: 180102,
	}
	got, err := applyPredicates(probe, []string{"rustc", "--new"}, []versionPredicate{{
		AtLeast:    "1.98.0",
		Add:        []string{"--current"},
		Remove:     []string{"--old"},
		ElseAdd:    []string{"--old"},
		ElseRemove: []string{"--new"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rustc", "--old"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestApplyConfigConditionsPreservesInsertionPoint(t *testing.T) {
	config := map[string]string{
		"CONFIG_PTR_AUTH": "y",
	}
	got, err := applyConfigConditions(config, []string{
		"rustc",
		"--target=aarch64-unknown-none",
		configFlagsMarker,
		"--crate-name=kernel",
	}, []configCondition{
		{
			Config:    "CONFIG_BTI",
			Flags:     []string{"-Zbranch-protection=bti,pac-ret"},
			ElseFlags: []string{"-Cforce-unwind-tables=n"},
		},
		{
			Config: "CONFIG_PTR_AUTH",
			Unless: "CONFIG_BTI",
			Flags:  []string{"-Zbranch-protection=pac-ret"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"rustc",
		"--target=aarch64-unknown-none",
		"-Cforce-unwind-tables=n",
		"-Zbranch-protection=pac-ret",
		"--crate-name=kernel",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestApplyPredicatesUsesResolvedConfig(t *testing.T) {
	probe := rusttoolchain.Probe{
		Schema:          rusttoolchain.ProbeSchema,
		VersionText:     "rustc 1.98.0-nightly (012345678 2026-06-24)",
		Release:         "1.98.0-nightly",
		Semver:          "1.98.0",
		Channel:         "nightly",
		CommitHash:      "0123456789",
		LLVMVersion:     "22.1.7",
		VersionCode:     109800,
		LLVMVersionCode: 220107,
	}
	predicates := []versionPredicate{
		{
			AtLeast: "1.98.0",
			Add:     []string{"--bti-version-flag"},
			Config:  "CONFIG_ARM64_BTI_KERNEL",
		},
		{
			AtLeast: "1.98.0",
			Add:     []string{"--ptr-auth-version-flag"},
			Config:  "CONFIG_ARM64_PTR_AUTH_KERNEL",
			Unless:  "CONFIG_ARM64_BTI_KERNEL",
		},
	}
	got, err := applyPredicatesWithConfig(probe, map[string]string{
		"CONFIG_ARM64_BTI_KERNEL":      "n",
		"CONFIG_ARM64_PTR_AUTH_KERNEL": "y",
	}, []string{"rustc"}, predicates)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rustc", "--ptr-auth-version-flag"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}
