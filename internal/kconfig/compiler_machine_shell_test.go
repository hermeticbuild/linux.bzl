package kconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxCompilerMachineShellEvaluatesSourceOwnedUpstreamMapping(t *testing.T) {
	command := `uname -m | sed -e s/i.86/x86/ -e s/x86_64/x86/ -e /^arm64$/!s/arm.*/arm/ -e s/aarch64.*/arm64/`
	for _, test := range []struct {
		machine string
		want    string
	}{
		{machine: "x86_64-linux-gnu", want: "x86"},
		{machine: "aarch64-linux-gnu", want: "arm64"},
		{machine: "armv7a-none-eabi", want: "arm"},
	} {
		got, handled, err := EvaluateLinuxCompilerMachineShell(command, test.machine, nil)
		if err != nil {
			t.Fatalf("machine %q: %v", test.machine, err)
		}
		if !handled || got != test.want {
			t.Fatalf("machine %q = %q, handled %v; want %q", test.machine, got, handled, test.want)
		}
	}
}

func TestLinuxCompilerMachineShellUsesConfiguredCompilerTripleWithoutFamilyKnowledge(t *testing.T) {
	const compiler = "__LINUX_BZL_KBUILD_ACTION_target_cc__"
	got, handled, err := EvaluateLinuxCompilerMachineShell(
		compiler+` -dumpmachine | cut -d- -f1 | tr A-Z a-z | sed 's/^vendor_//' | head -n 1`,
		"VENDOR_RISCV64-acme-elf",
		[]string{compiler},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || got != "riscv64" {
		t.Fatalf("compiler-machine result = %q, handled %v; want riscv64", got, handled)
	}
}

func TestLinuxCompilerMachineShellLeavesUnrelatedCommandsToCaller(t *testing.T) {
	got, handled, err := EvaluateLinuxCompilerMachineShell("getconf LFS_CFLAGS", "x86_64-linux-gnu", []string{"cc"})
	if err != nil || handled || got != "" {
		t.Fatalf("unrelated command = %q, handled %v, err %v", got, handled, err)
	}
}

func TestLinuxCompilerMachineShellFailsClosedAfterRecognizedRoot(t *testing.T) {
	for _, command := range []string{
		"uname -m | vendor-filter",
		"uname -m && printf x86",
		"uname -m |",
		"cc -dumpmachine | sed -n p",
	} {
		_, handled, err := EvaluateLinuxCompilerMachineShell(command, "x86_64-linux-gnu", []string{"cc"})
		if !handled || err == nil {
			t.Fatalf("command %q handled=%v err=%v, want handled error", command, handled, err)
		}
	}
}

func TestLinuxCompilerMachineShellRejectsMalformedInputs(t *testing.T) {
	for _, test := range []struct {
		name, command, machine string
	}{
		{name: "machine", command: "uname -m", machine: "x86_64 linux"},
		{name: "quote", command: `uname -m | sed 's/x86/x86_64/`, machine: "x86_64-linux-gnu"},
		{name: "sed regex", command: `uname -m | sed 's/[//g'`, machine: "x86_64-linux-gnu"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := EvaluateLinuxCompilerMachineShell(test.command, test.machine, nil)
			if err == nil {
				t.Fatal("malformed input unexpectedly succeeded")
			}
		})
	}
}

func TestLinuxCompilerMachineShellContainsNoArchitectureMappingTable(t *testing.T) {
	// An unlisted target passes through the source's identity mapping exactly;
	// the helper must never reject or reinterpret it based on a built-in list.
	const machine = "quantum128-vendor-none"
	got, handled, err := EvaluateLinuxCompilerMachineShell("uname -m", machine, nil)
	if err != nil || !handled || got != strings.SplitN(machine, "-", 2)[0] {
		t.Fatalf("unlisted machine = %q, handled %v, err %v", got, handled, err)
	}
}

func TestLinuxCompilerMachineShellDrivesVendorMakeLayout(t *testing.T) {
	root := t.TempDir()
	vendor := filepath.Join(root, "vendor", "platform")
	if err := os.MkdirAll(vendor, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendor, "target-identity.mk"), []byte(`
raw_target := $(shell $(CC) -dumpmachine)
vendor_cpu := $(word 1,$(subst -, ,$(raw_target)))
ifeq ($(vendor_cpu),VENDOR_RISCV64)
vendor_cpu := riscv
endif
SELECTED_KERNEL_ARCH := $(vendor_cpu)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
include $(srctree)/vendor/platform/target-identity.mk
BUILD_ARCH ?= $(SELECTED_KERNEL_ARCH)
UTS_MACHINE := vendor-$(BUILD_ARCH)
SRCARCH := $(BUILD_ARCH)
ARCH := $(BUILD_ARCH)

# An unrelated dynamic value after the identity must remain lazy.
UNUSED_DYNAMIC := $(shell undeclared-vendor-discovery)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	const compiler = "__LINUX_BZL_KBUILD_ACTION_target_cc__"
	parsed, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), KbuildOptions{
		RootDir:               root,
		Variables:             map[string]string{"srctree": root},
		CommandLineVariables:  map[string]string{"CC": compiler},
		MakeVariablesComplete: true,
		CaptureVariables:      []string{"ARCH", "SRCARCH", "UTS_MACHINE"},
		SkipExportedVariables: true,
		Shell: func(command string) (string, error) {
			value, handled, err := EvaluateLinuxCompilerMachineShell(
				command,
				"VENDOR_RISCV64-acme-elf",
				[]string{compiler},
			)
			if err != nil {
				return "", err
			}
			if !handled {
				return "", os.ErrPermission
			}
			return value, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"ARCH": "riscv", "SRCARCH": "riscv", "UTS_MACHINE": "vendor-riscv",
	} {
		if got := parsed.Variables[name]; got != want {
			t.Errorf("vendor %s=%q, want %q", name, got, want)
		}
	}
}

func TestLinuxCompilerMachineShellRejectsDynamicVendorArchitectureTool(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
ARCH := $(shell vendor-architecture-probe)
SRCARCH := $(ARCH)
UTS_MACHINE := $(ARCH)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), KbuildOptions{
		RootDir:               root,
		MakeVariablesComplete: true,
		CaptureVariables:      []string{"ARCH", "SRCARCH", "UTS_MACHINE"},
		SkipExportedVariables: true,
		Shell: func(command string) (string, error) {
			value, handled, err := EvaluateLinuxCompilerMachineShell(command, "x86_64-acme-linux", nil)
			if err != nil {
				return "", err
			}
			if !handled {
				return "", os.ErrPermission
			}
			return value, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("dynamic vendor architecture command error = %v", err)
	}
}
