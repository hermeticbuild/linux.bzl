package main

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestAssemblyWrapper(t *testing.T) {
	got := assemblyWrapper(
		"bazel-out/pm/bin/kernel/empty_root.dtb",
		".rodata",
		"__dtb_empty_root",
	)
	for _, want := range []string{
		`.section .rodata,"a"`,
		`.global __dtb_empty_root_begin`,
		`.incbin "bazel-out/pm/bin/kernel/empty_root.dtb"`,
		`.global __dtb_empty_root_end`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("assemblyWrapper() missing %q:\n%s", want, got)
		}
	}
}

func TestEmptyRootDTBLinux618(t *testing.T) {
	dtb, err := emptyRootDTB(`/dts-v1/;
/ {
	/*
	 * The current source spells out both required root cell properties.
	 */
	#address-cells = <0x02>;
	#size-cells = <0x02>;
};`)
	if err != nil {
		t.Fatalf("emptyRootDTB() failed: %v", err)
	}
	if got := binary.BigEndian.Uint32(dtb[0:4]); got != fdtMagic {
		t.Fatalf("magic = 0x%x, want 0x%x", got, fdtMagic)
	}
	if got := binary.BigEndian.Uint32(dtb[4:8]); got != uint32(len(dtb)) {
		t.Fatalf("totalsize = %d, want %d", got, len(dtb))
	}
	if got := binary.BigEndian.Uint32(dtb[32:36]); got != 27 {
		t.Fatalf("size_dt_strings = %d, want 27", got)
	}
	if got := string(dtb[len(dtb)-27:]); got != "#address-cells\x00#size-cells\x00" {
		t.Fatalf("strings block suffix = %q", got)
	}
}

func TestEmptyRootDTBLinux612(t *testing.T) {
	dtb, err := emptyRootDTB(`// SPDX-License-Identifier: GPL-2.0-only
/dts-v1/;

/ {

};`)
	if err != nil {
		t.Fatalf("emptyRootDTB() failed: %v", err)
	}
	if got, want := len(dtb), 72; got != want {
		t.Fatalf("DTB size = %d, want %d", got, want)
	}
	for offset, want := range map[int]uint32{
		0:  fdtMagic,
		4:  uint32(len(dtb)),
		8:  56,
		12: 72,
		16: 40,
		32: 0,
		36: 16,
		56: fdtBeginNode,
		64: fdtEndNode,
		68: fdtEnd,
	} {
		if got := binary.BigEndian.Uint32(dtb[offset : offset+4]); got != want {
			t.Fatalf("word at offset %d = 0x%x, want 0x%x", offset, got, want)
		}
	}
}

func TestEmptyRootDTBRejectsPropertiesInComments(t *testing.T) {
	_, err := emptyRootDTB(`/dts-v1/;
/ {
	// #address-cells = <0x02>;
	// #size-cells = <0x02>;
	compatible = "not-empty";
};`)
	if err == nil || !strings.Contains(err.Error(), "unsupported empty root DTS structure") {
		t.Fatalf("emptyRootDTB() error = %v, want unsupported structure", err)
	}
}

func TestEmptyRootDTBRejectsUnterminatedComment(t *testing.T) {
	_, err := emptyRootDTB(`/dts-v1/;
/ {
	/*
};`)
	if err == nil || !strings.Contains(err.Error(), "unterminated block comment") {
		t.Fatalf("emptyRootDTB() error = %v, want unterminated block comment", err)
	}
}
