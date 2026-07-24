package main

import (
	"debug/elf"
	"strings"
	"testing"
)

func TestVDSOSymbolFieldsTracksKernelStruct(t *testing.T) {
	fields, err := vdsoSymbolFields([]byte(`
struct vdso_image {
	long sym_vvar_start;
	long sym_vvar_page;
	long sym___kernel_vsyscall;
};
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vvar_start", "vvar_page", "__kernel_vsyscall"} {
		if !fields[name] {
			t.Errorf("field %q was not detected", name)
		}
	}
	if fields["pvclock_page"] {
		t.Error("field pvclock_page was detected despite being absent")
	}
}

func TestVDSOSymbolFieldsRejectsUnknownStruct(t *testing.T) {
	_, err := vdsoSymbolFields([]byte("struct vdso_image { void *data; };"))
	if err == nil || !strings.Contains(err.Error(), "no recognized symbol fields") {
		t.Fatalf("vdsoSymbolFields() error = %v", err)
	}
}

func TestValidateVvarSymbols(t *testing.T) {
	fields := map[string]bool{"vvar_start": true}
	valid := map[string]int64{
		"vvar_start":   -4 * 4096,
		"vvar_page":    -4 * 4096,
		"pvclock_page": -3 * 4096,
		"hvclock_page": -2 * 4096,
		"timens_page":  -1 * 4096,
	}
	if err := validateVvarSymbols(valid, fields); err != nil {
		t.Fatalf("validateVvarSymbols() error = %v", err)
	}

	tests := []struct {
		name    string
		symbols map[string]int64
		want    string
	}{
		{
			name:    "missing start",
			symbols: map[string]int64{},
			want:    "no vvar_start",
		},
		{
			name: "unaligned page",
			symbols: map[string]int64{
				"vvar_start": -4 * 4096,
				"vvar_page":  -4097,
			},
			want: "multiple of 4096",
		},
		{
			name: "page below range",
			symbols: map[string]int64{
				"vvar_start": -4 * 4096,
				"vvar_page":  -6 * 4096,
			},
			want: "underruns vvar_start",
		},
		{
			name: "page above text",
			symbols: map[string]int64{
				"vvar_start": -4 * 4096,
				"vvar_page":  4096,
			},
			want: "wrong side",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateVvarSymbols(test.symbols, fields)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateVvarSymbols() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateVvarSymbolsSkipsNewKernelLayout(t *testing.T) {
	if err := validateVvarSymbols(nil, map[string]bool{}); err != nil {
		t.Fatalf("validateVvarSymbols() error = %v", err)
	}
}

func TestWriteCOnlyEmitsKernelStructFields(t *testing.T) {
	var out strings.Builder
	writeC(
		&out,
		nil,
		[]byte{0},
		&elf.File{},
		"vdso_image_64",
		map[string]int64{
			"vvar_start":        -4 * 4096,
			"pvclock_page":      -3 * 4096,
			"__kernel_vsyscall": 512,
		},
		map[string]bool{
			"vvar_start":        true,
			"__kernel_vsyscall": true,
		},
	)
	generated := out.String()
	for _, want := range []string{
		".sym_vvar_start = -16384",
		".sym___kernel_vsyscall = 512",
	} {
		if !strings.Contains(generated, want) {
			t.Errorf("generated output does not contain %q", want)
		}
	}
	if strings.Contains(generated, ".sym_pvclock_page") {
		t.Error("generated output contains a symbol absent from the kernel struct")
	}
}
