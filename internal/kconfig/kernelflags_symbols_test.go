package kconfig

import "testing"

func TestKernelFlagsConfigSymbolsScopesArchitectureDependencies(t *testing.T) {
	tests := []struct {
		arch     string
		want     string
		unwanted string
	}{
		{arch: "arm", want: "CONFIG_AEABI", unwanted: "CONFIG_RISCV_ISA_C"},
		{arch: "powerpc", want: "CONFIG_PPC64_ELF_ABI_V2", unwanted: "CONFIG_AEABI"},
		{arch: "riscv", want: "CONFIG_RISCV_ISA_C", unwanted: "CONFIG_PPC64_ELF_ABI_V2"},
	}
	for _, tt := range tests {
		t.Run(tt.arch, func(t *testing.T) {
			got := map[string]bool{}
			for _, symbol := range KernelFlagsConfigSymbols(tt.arch) {
				got[symbol] = true
			}
			if !got[tt.want] {
				t.Errorf("compiler footprint is missing %s", tt.want)
			}
			if got[tt.unwanted] {
				t.Errorf("compiler footprint unexpectedly includes %s", tt.unwanted)
			}
		})
	}
}
