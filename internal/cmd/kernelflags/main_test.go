// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	_ "embed"
	"regexp"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

//go:embed main.go
var kernelflagsSource string

func TestKernelFlagsConfigSymbolsCoverSource(t *testing.T) {
	known := map[string]bool{}
	for _, symbol := range kconfig.KernelFlagsConfigSymbols() {
		known[symbol] = true
	}
	for _, token := range regexp.MustCompile(`CONFIG_[A-Z0-9_]+`).FindAllString(kernelflagsSource, -1) {
		if !known[token] {
			t.Errorf("kernelflags reads %s but it is absent from kconfig.KernelFlagsConfigSymbols()", token)
		}
	}
}

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

func TestLinuxCFlagsX86ExcludeGCCOnlyAlignmentFlag(t *testing.T) {
	flags := linuxCFlags(map[string]string{"CONFIG_X86_64": "y"}, "x86")
	if contains(flags, "-falign-jumps=1") {
		t.Fatalf("linuxCFlags() contains GCC-only -falign-jumps=1: %v", flags)
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

func TestLinuxCFlagsUseO2ForPerformanceOptimization(t *testing.T) {
	config := map[string]string{
		"CONFIG_CC_OPTIMIZE_FOR_PERFORMANCE": "y",
	}
	flags := linuxCFlags(config, "arm64")
	if !contains(flags, "-O2") {
		t.Fatalf("linuxCFlags() missing -O2: %v", flags)
	}
	if contains(flags, "-Os") {
		t.Fatalf("linuxCFlags() unexpectedly contains -Os: %v", flags)
	}
}

func TestLinuxCFlagsUseOsForSizeOptimization(t *testing.T) {
	config := map[string]string{
		"CONFIG_CC_OPTIMIZE_FOR_SIZE": "y",
	}
	flags := linuxCFlags(config, "arm64")
	if !contains(flags, "-Os") {
		t.Fatalf("linuxCFlags() missing -Os: %v", flags)
	}
	if contains(flags, "-O2") {
		t.Fatalf("linuxCFlags() unexpectedly contains -O2: %v", flags)
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
