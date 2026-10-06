package main

import (
	"bufio"
	"strings"
	"testing"
)

// TestGfmulMatchesUpstream pins the Galois-field arithmetic. The reduction
// polynomial and the carry handling are the only places a port can silently
// differ while still producing plausible-looking tables.
func TestGfmulMatchesUpstream(t *testing.T) {
	for _, tc := range []struct {
		a, b, want uint8
	}{
		{a: 0, b: 0, want: 0},
		{a: 1, b: 1, want: 1},
		{a: 2, b: 2, want: 4},
		{a: 0x80, b: 2, want: 0x1d},
		{a: 0xff, b: 0xff, want: 0xe2},
		{a: 0x53, b: 0x8c, want: 0x01},
	} {
		if got := gfmul(tc.a, tc.b); got != tc.want {
			t.Errorf("gfmul(%#02x, %#02x) = %#02x, want %#02x", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestGfpowInverse checks x * x^254 == 1 for every non-zero element, which is
// the property the raid6_gfinv table exists to provide.
func TestGfpowInverse(t *testing.T) {
	for x := 1; x < 256; x++ {
		inverse := gfpow(uint8(x), 254)
		if got := gfmul(uint8(x), inverse); got != 1 {
			t.Fatalf("%#02x * inv(%#02x)=%#02x = %#02x, want 0x01", x, x, inverse, got)
		}
	}
}

// TestWriteTablesShape pins the structure of the generated file against the
// upstream program's output. The full output was additionally compared
// byte-for-byte with a compiled lib/raid6/mktables.c; this keeps the
// load-bearing parts checked in CI, which has no C hostprog to compare against.
func TestWriteTablesShape(t *testing.T) {
	var out strings.Builder
	w := bufio.NewWriter(&out)
	writeTables(w)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush() failed: %v", err)
	}
	generated := out.String()

	for _, want := range []string{
		"#include <linux/raid/pq.h>\n",
		"const u8  __attribute__((aligned(256)))\nraid6_gfmul[256][256] =\n{\n",
		"const u8  __attribute__((aligned(256)))\nraid6_vgfmul[256][32] =\n{\n",
		"const u8 __attribute__((aligned(256)))\nraid6_gfexp[256] =\n{\n",
		"const u8 __attribute__((aligned(256)))\nraid6_gflog[256] =\n{\n",
		"const u8 __attribute__((aligned(256)))\nraid6_gfinv[256] =\n{\n",
		"const u8 __attribute__((aligned(256)))\nraid6_gfexi[256] =\n{\n",
		"EXPORT_SYMBOL(raid6_gfmul);\n",
		"EXPORT_SYMBOL(raid6_gfexi);\n",
	} {
		if !strings.Contains(generated, want) {
			t.Errorf("generated tables are missing %q", want)
		}
	}

	// The first row of raid6_gfmul is multiplication by zero, and the second
	// is the identity. Both are easy to get wrong and obvious once checked.
	if !strings.Contains(generated, "\t{\n\t\t0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,\n") {
		t.Errorf("raid6_gfmul[0] is not all zeroes")
	}
	if !strings.Contains(generated, "\t\t0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,\n") {
		t.Errorf("raid6_gfmul[1] is not the identity")
	}

	if lines := strings.Count(generated, "\n"); lines != 10420 {
		t.Errorf("generated tables have %d lines, want 10420", lines)
	}
	if size := len(generated); size != 471449 {
		t.Errorf("generated tables are %d bytes, want 471449", size)
	}
}
