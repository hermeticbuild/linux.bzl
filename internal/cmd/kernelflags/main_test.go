// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestLinuxAFlagsX86IncludeFtraceUsingDefines(t *testing.T) {
	config := map[string]string{
		"CONFIG_FUNCTION_TRACER":      "y",
		"CONFIG_FTRACE_MCOUNT_USE_CC": "y",
		"CONFIG_HAVE_FENTRY":          "y",
		"CONFIG_X86_64":               "y",
	}
	flags := linuxAFlags(config, "x86")
	if !contains(flags, "-DCC_USING_FENTRY") {
		t.Fatalf("linuxAFlags() missing -DCC_USING_FENTRY: %v", flags)
	}
	if contains(flags, "-mfentry") {
		t.Fatalf("linuxAFlags() leaked C-only -mfentry into assembly flags: %v", flags)
	}
}

func TestLinuxCFlagsX86IncludeFtraceCompilerAndUsingFlags(t *testing.T) {
	config := map[string]string{
		"CONFIG_FUNCTION_TRACER":      "y",
		"CONFIG_FTRACE_MCOUNT_USE_CC": "y",
		"CONFIG_HAVE_FENTRY":          "y",
		"CONFIG_X86_64":               "y",
	}
	flags := linuxCFlags(config, "x86")
	for _, want := range []string{"-pg", "-mrecord-mcount", "-mfentry", "-DCC_USING_FENTRY"} {
		if !contains(flags, want) {
			t.Fatalf("linuxCFlags() missing %s: %v", want, flags)
		}
	}
}

func TestLinuxCFlagsIncludeClangThinLTO(t *testing.T) {
	config := map[string]string{
		"CONFIG_CC_IS_CLANG":    "y",
		"CONFIG_LTO_CLANG_THIN": "y",
	}
	flags := linuxCFlags(config, "arm64")
	for _, want := range []string{"-fno-lto", "-flto=thin", "-fsplit-lto-unit", "-fvisibility=hidden"} {
		if !contains(flags, want) {
			t.Fatalf("linuxCFlags() missing %s: %v", want, flags)
		}
	}
}

func TestLinuxCFlagsIncludeClangFullLTO(t *testing.T) {
	config := map[string]string{
		"CONFIG_CC_IS_CLANG":    "y",
		"CONFIG_LTO_CLANG_FULL": "y",
	}
	flags := linuxCFlags(config, "arm64")
	for _, want := range []string{"-fno-lto", "-flto", "-fvisibility=hidden"} {
		if !contains(flags, want) {
			t.Fatalf("linuxCFlags() missing %s: %v", want, flags)
		}
	}
}

func TestLinuxCFlagsUseSectionSplittingForDeadCodeElimination(t *testing.T) {
	config := map[string]string{
		"CONFIG_LD_DEAD_CODE_DATA_ELIMINATION": "y",
	}
	flags := linuxCFlags(config, "arm64")
	for _, want := range []string{"-ffunction-sections", "-fdata-sections"} {
		if !contains(flags, want) {
			t.Fatalf("linuxCFlags() missing %s: %v", want, flags)
		}
	}
	for _, unwanted := range []string{"-fno-function-sections", "-fno-data-sections"} {
		if contains(flags, unwanted) {
			t.Fatalf("linuxCFlags() unexpectedly contains %s: %v", unwanted, flags)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
