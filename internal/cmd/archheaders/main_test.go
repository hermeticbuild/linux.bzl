package main

import (
	"debug/elf"
	"strings"
	"testing"
)

func TestGenerateMachTypes(t *testing.T) {
	out, err := generateMachTypes([]byte("board ARCH_BOARD BOARD 42\nfuture ARCH_FUTURE FUTURE\n"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{"MACH_TYPE_BOARD", "machine_is_board", "machine_is_future()\t(0)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output does not contain %q:\n%s", want, text)
		}
	}
}

func TestGenerateARMSyscallNR(t *testing.T) {
	out, err := generateARMSyscallNR([]byte("0 common read sys_read\n511 common last sys_last\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "#define __NR_syscalls 512") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestGenerateArchitectureCFlags(t *testing.T) {
	tests := []struct {
		arch    string
		config  map[string]string
		offsets string
		want    []string
	}{
		{"arm", map[string]string{"CONFIG_STACKPROTECTOR_PER_TASK": "y", "CONFIG_CC_HAVE_STACKPROTECTOR_TLS": "y"}, "#define TSK_STACK_CANARY 12\n", []string{"-mstack-protector-guard=tls", "offset=12"}},
		{"riscv", map[string]string{"CONFIG_STACKPROTECTOR_PER_TASK": "y"}, "#define TSK_STACK_CANARY 24\n", []string{"reg=tp", "offset=24"}},
		{"powerpc", map[string]string{"CONFIG_STACKPROTECTOR": "y"}, "#define PACA_CANARY 40\n", []string{"reg=r13", "offset=40"}},
	}
	for _, test := range tests {
		t.Run(test.arch, func(t *testing.T) {
			out, err := generateArchitectureCFlags(test.arch, test.config, []byte(test.offsets))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range test.want {
				if !strings.Contains(string(out), want) {
					t.Fatalf("output %q does not contain %q", out, want)
				}
			}
		})
	}
}

func TestARMKernelBSSSize(t *testing.T) {
	size, err := armKernelBSSSize([]elf.Symbol{
		{Name: "__bss_stop", Value: 0x1240},
		{Name: "unrelated", Value: 7},
		{Name: "__bss_start", Value: 0x1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if size != 0x240 {
		t.Fatalf("armKernelBSSSize() = %#x, want 0x240", size)
	}
}

func TestARMKernelBSSSizeRejectsInvalidSymbols(t *testing.T) {
	for _, symbols := range [][]elf.Symbol{
		{{Name: "__bss_start", Value: 1}},
		{{Name: "__bss_start", Value: 2}, {Name: "__bss_stop", Value: 1}},
	} {
		if _, err := armKernelBSSSize(symbols); err == nil {
			t.Fatalf("armKernelBSSSize(%v) unexpectedly succeeded", symbols)
		}
	}
}

func TestARMTextOffset(t *testing.T) {
	tests := []struct {
		config map[string]string
		want   uint64
	}{
		{map[string]string{}, 0x00008000},
		{map[string]string{"CONFIG_ARCH_REALTEK": "y"}, 0x00108000},
		{map[string]string{"CONFIG_ARCH_MESON": "y"}, 0x00208000},
		{map[string]string{"CONFIG_ARCH_AXXIA": "y"}, 0x00308000},
	}
	for _, test := range tests {
		if got := armTextOffset(test.config); got != test.want {
			t.Errorf("armTextOffset(%v) = %#x, want %#x", test.config, got, test.want)
		}
	}
}
