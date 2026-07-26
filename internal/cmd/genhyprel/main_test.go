package main

import "testing"

func TestRelocationTypeConstants(t *testing.T) {
	if rAArch64Abs64 != 257 {
		t.Fatalf("R_AARCH64_ABS64 = %d, want 257", rAArch64Abs64)
	}
	if rAArch64Prel32 != 261 {
		t.Fatalf("R_AARCH64_PREL32 = %d, want 261", rAArch64Prel32)
	}
	if rAArch64Call26 != 283 {
		t.Fatalf("R_AARCH64_CALL26 = %d, want 283", rAArch64Call26)
	}
}
