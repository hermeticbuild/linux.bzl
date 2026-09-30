package kconfig

import (
	"strings"
	"testing"
)

func TestExternalKbuildCommandOperandScopesOnlyParentRelativePaths(t *testing.T) {
	const directory = ".linux-bzl/external/hello_c_module_armv7"
	probeToken := "LINUX_BZL_PROBE_" + strings.Repeat("a", 64)

	profile := mustCompactKbuildProfileForTest(
		t,
		"external-probe-operand",
		"scripts/Makefile.build",
		directory,
		"",
		nil,
	)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree:      CompactKbuildInvocationObjectTree,
		Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		value string
		want  string
		ok    bool
	}{
		{
			name:  "symbolic probe remains graph rooted",
			value: probeToken,
			want:  probeToken,
			ok:    true,
		},
		{
			name:  "embedded symbolic probe remains graph rooted",
			value: "rust/" + probeToken,
			want:  "rust/" + probeToken,
			ok:    true,
		},
		{
			name:  "symbolic probe option is not newly classified",
			value: "--flag=" + probeToken,
			ok:    false,
		},
		{
			name:  "ordinary nested operand remains graph rooted",
			value: "include/config/auto.conf",
			want:  "include/config/auto.conf",
			ok:    true,
		},
		{
			name:  "dot relative operand keeps legacy canonical form",
			value: "./module.c",
			want:  "module.c",
			ok:    true,
		},
		{
			name:  "parent relative operand uses invocation directory",
			value: "../../../include/generated/asm-offsets.h",
			want:  "include/generated/asm-offsets.h",
			ok:    true,
		},
		{
			name:  "parent relative option value uses invocation directory",
			value: "--input=../../../include/generated/asm-offsets.h",
			want:  "include/generated/asm-offsets.h",
			ok:    true,
		},
		{
			name:  "ordinary option value is not newly classified",
			value: "--input=include/config/auto.conf",
			ok:    false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := compactKbuildProfileCommandOperandPath(profile, test.value)
			if got != test.want || ok != test.ok {
				t.Fatalf(
					"compactKbuildProfileCommandOperandPath(%q) = (%q, %t), want (%q, %t)",
					test.value, got, ok, test.want, test.ok,
				)
			}
			if ok && strings.Contains(test.value, probeToken) && strings.HasPrefix(got, directory+"/") {
				t.Fatalf("symbolic probe token was incorrectly scoped to external invocation directory: %q", got)
			}
		})
	}
}
