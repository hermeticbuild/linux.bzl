// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestLinuxAFlagsX86IncludeFtraceUsingDefines(t *testing.T) {
	config := map[string]string{
		"CONFIG_FUNCTION_TRACER": "y",
		"CONFIG_HAVE_FENTRY":     "y",
		"CONFIG_X86_64":          "y",
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
		"CONFIG_FUNCTION_TRACER": "y",
		"CONFIG_HAVE_FENTRY":     "y",
		"CONFIG_X86_64":          "y",
	}
	flags := linuxCFlags(config, "x86")
	for _, want := range []string{"-mfentry", "-DCC_USING_FENTRY"} {
		if !contains(flags, want) {
			t.Fatalf("linuxCFlags() missing %s: %v", want, flags)
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
