package kconfig

import (
	"slices"
	"testing"
)

func TestCompactKbuildAwkConfigProjectionProofAcceptsCpuFeatureMaskShape(t *testing.T) {
	source := []byte(`#!/usr/bin/awk
BEGIN {
	printf "#ifndef _MASKS_H\n";
	printf "#define _MASKS_H\n";
	file = 0
}
FNR == 1 {
	++file;
	if (file == 1)
		FS = "[ \t()*+]+";
	if (file == 2)
		FS = "=";
}
file == 1 && $1 ~ /^#define$/ && $2 ~ /^FEATURE_/ {
	features[$3] = $2;
}
file == 2 && $1 ~ /^CONFIG_X86_(REQUIRED|DISABLED)_FEATURE_/ {
	on = ($2 == "y");
	if (split($1, fields, "CONFIG_X86_|_FEATURE_") == 3)
		selected[fields[2], fields[3]] = on;
}
END {
	for (kind in selected)
		printf "#define %s_MASK 0x%08xU\n", kind, int(selected[kind]);
	printf "#define MASK_BIT_SET(x) ((x) & 1U)\n";
	printf "#endif\n";
}
`)
	got := compactKbuildAwkConfigProjectionProof(source, 2)
	want := []string{"CONFIG_X86_DISABLED_FEATURE_", "CONFIG_X86_REQUIRED_FEATURE_"}
	if !slices.Equal(got, want) {
		rules, parsed := compactKbuildAwkRules(source)
		t.Logf("rules parsed=%t calls_closed=%t rules=%#v", parsed, compactKbuildAwkCallsAreClosed(source), rules)
		t.Fatalf("config selector prefixes = %q, want %q", got, want)
	}
	if !compactKbuildLiteralClosedMacroEmitterEvidence(source) {
		t.Fatal("cpu-feature-shaped source lacks closed macro emitter evidence")
	}
}

func TestCompactKbuildAwkConfigProjectionProofRejectsDynamicConfigRecordRule(t *testing.T) {
	source := []byte(`BEGIN { file = 0 }
FNR == 1 { ++file }
file == 2 && $1 ~ selector { value[$1] = $2 }
END { printf "#ifndef _H\n#define _H\n#define V 1U\n#endif\n" }
`)
	if got := compactKbuildAwkConfigProjectionProof(source, 2); len(got) != 0 {
		t.Fatalf("dynamic selector proof = %q, want ineligible", got)
	}
}
