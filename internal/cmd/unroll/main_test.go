package main

import "testing"

// TestUnrollMatchesAwkFilter pins the semantics of lib/raid6/unroll.awk.
//
// CI has no awk toolchain, so these expectations were produced by running GNU
// awk 5.3.2 with the upstream script and pasted in. The port was additionally
// checked byte-for-byte against awk over both catalogued kernels' five ".uc"
// templates at every unroll factor the tree uses.
func TestUnrollMatchesAwkFilter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		n     int
		want  string
	}{
		{
			name:  "repeats lines containing the index marker",
			input: "before\nwx$$ = *(unative_t *)&dptr[z0][d+$$*NSIZE];\nafter\n",
			n:     2,
			want: "before\n" +
				"wx0 = *(unative_t *)&dptr[z0][d+0*NSIZE];\n" +
				"wx1 = *(unative_t *)&dptr[z0][d+1*NSIZE];\n" +
				"after\n",
		},
		{
			name:  "substitutes the unroll count",
			input: "#define NSIZE $#\n",
			n:     4,
			want:  "#define NSIZE 4\n",
		},
		{
			name: "emits a literal dollar for the escape",
			// "$*" becomes a bare "$". It has to be substituted after "$$"
			// and "$#", or the "$" it produces gets rewritten again.
			input: "/* $* */\n",
			n:     2,
			want:  "/* $ */\n",
		},
		{
			name:  "substitution order leaves generated dollars alone",
			input: "$*$$ $*$#\n",
			n:     1,
			want:  "$0 $1\n",
		},
		{
			name: "drops repeated lines entirely when the count is zero",
			// Upstream behaviour: the for loop body never runs. Preserve it.
			input: "keep\nrepeat $$\nkeep2\n",
			n:     0,
			want:  "keep\nkeep2\n",
		},
		{
			name: "a trailing newline terminates the last record",
			// awk's print always appends ORS, and the final newline in the
			// input ends a record rather than starting an empty one. Getting
			// this wrong inserts a blank line inside a function body.
			input: "a\nb\n",
			n:     1,
			want:  "a\nb\n",
		},
		{
			name:  "input without a trailing newline still gets one",
			input: "a\nb",
			n:     1,
			want:  "a\nb\n",
		},
		{
			name:  "empty input produces no output",
			input: "",
			n:     8,
			want:  "",
		},
		{
			name:  "blank lines are preserved",
			input: "a\n\nb\n",
			n:     1,
			want:  "a\n\nb\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unroll(tc.input, tc.n); got != tc.want {
				t.Errorf("unroll(%q, %d)\nwant: %q\n got: %q", tc.input, tc.n, tc.want, got)
			}
		})
	}
}
