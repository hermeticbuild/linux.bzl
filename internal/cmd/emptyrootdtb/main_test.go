// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/binary"
	"testing"
)

func TestEmptyRootDTB(t *testing.T) {
	dtb, err := emptyRootDTB(`/dts-v1/;
/ {
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
