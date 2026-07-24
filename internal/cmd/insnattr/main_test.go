package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvalid64Compatibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		support bool
		want    string
	}{
		{name: "legacy", support: false, want: ""},
		{name: "current", support: true, want: "INAT_INV64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "inat-tables.c")
			output, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			g := newGenerator(output, tc.support)
			g.line = 2
			g.tname = "inat_primary_table"
			if err := g.opcodeLine("06: PUSH ES (i64)", []string{"06:", "PUSH", "ES", "(i64)"}); err != nil {
				t.Fatal(err)
			}
			if err := output.Close(); err != nil {
				t.Fatal(err)
			}
			if got := g.table["0x06"]; got != tc.want {
				t.Fatalf("table[0x06] = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestXOPTablesFollowOpcodeMap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  bool
	}{
		{
			name: "legacy",
			input: `Table: primary
Referrer:
AVXcode:
01: ADD Eb,Gb
EndTable
`,
		},
		{
			name: "current",
			input: `Table: XOP map 8h
Referrer:
XOPcode: 0
85: VPMACSSWW Vo,Ho,Wo,Lo
EndTable
`,
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in := filepath.Join(dir, "x86-opcode-map.txt")
			out := filepath.Join(dir, "inat-tables.c")
			if err := os.WriteFile(in, []byte(tc.input), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := run(in, out, false); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(data), "inat_xop_tables"); got != tc.want {
				t.Fatalf("inat_xop_tables present = %t, want %t\n%s", got, tc.want, data)
			}
		})
	}
}

func TestHeaderDefines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inat.h")
	if err := os.WriteFile(path, []byte("#define INAT_INV64 (1 << 2)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := headerDefines(path, "INAT_INV64")
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("headerDefines did not find INAT_INV64")
	}
}
