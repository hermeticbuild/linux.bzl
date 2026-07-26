package main

import (
	"strings"
	"testing"
)

func TestGenerateCPUCapDefs(t *testing.T) {
	out, err := generateCPUCapDefs([]byte(`
# comment
ALWAYS_BOOT
HAS_ARMv8_4_TTL
`))
	if err != nil {
		t.Fatalf("generateCPUCapDefs() failed: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"#define ARM64_ALWAYS_BOOT",
		"#define ARM64_HAS_ARMv8_4_TTL",
		"#define ARM64_NCAPS",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

func TestGenerateSysregDefs(t *testing.T) {
	out, err := generateSysregDefs([]byte(`
SysregFields	FIELDS
Field	63:32	Hi
Res0	31:16
UnsignedEnum	15:8	Mode
0b00000001	One
EndEnum
Unkn	7:0
EndSysregFields

Sysreg	TEST_EL1	3	0	1	2	3
Fields	FIELDS
EndSysreg
`))
	if err != nil {
		t.Fatalf("generateSysregDefs() failed: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"#define FIELDS_Hi",
		"#define FIELDS_Mode_One",
		"#define FIELDS_RES0",
		"#define REG_TEST_EL1",
		"/* For TEST_EL1 fields see FIELDS */",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

func TestGenerateSysregDefsLinux612EnumAliases(t *testing.T) {
	out, err := generateSysregDefs([]byte(`
Sysreg	ID_PFR1_EL1	3	0	0	1	1
Res0	63:8
Enum	7:4	Security
0b0000	NI
0b0001	EL3
0b0001	NSACR_RFR
EndEnum
Res0	3:0
EndSysreg

Sysreg	ID_AA64SMFR0_EL1	3	0	0	4	5
Res0	63:60
UnsignedEnum	59:56	SMEver
0b0000	SME
0b0001	SME2
0b0010	SME2p1
0b0000	IMP
EndEnum
Res0	55:0
EndSysreg
`))
	if err != nil {
		t.Fatalf("generateSysregDefs() failed: %v", err)
	}
	defines := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "#define" {
			defines[fields[1]] = fields[2]
		}
	}
	for name, want := range map[string]string{
		"ID_PFR1_EL1_Security_EL3":       "UL(0b0001)",
		"ID_PFR1_EL1_Security_NSACR_RFR": "UL(0b0001)",
		"ID_AA64SMFR0_EL1_SMEver_SME":    "UL(0b0000)",
		"ID_AA64SMFR0_EL1_SMEver_IMP":    "UL(0b0000)",
	} {
		if got := defines[name]; got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestGenerateSysregDefsRejectsOtherDuplicateEnumValues(t *testing.T) {
	_, err := generateSysregDefs([]byte(`
Sysreg	TEST_EL1	3	0	1	2	3
Res0	63:4
Enum	3:0	Mode
0b0001	One
0b0001	Alias
EndEnum
EndSysreg
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate Enum value 0b0001 for Alias") {
		t.Fatalf("generateSysregDefs() error = %v, want duplicate Enum value", err)
	}
}

func TestGenerateVDSOOffsets(t *testing.T) {
	out, err := generateVDSOOffsets([]byte("0000000000000420 T VDSO_clock_gettime\n0000000000000000 A ignored\n"))
	if err != nil {
		t.Fatalf("generateVDSOOffsets() failed: %v", err)
	}
	if got, want := string(out), "#define vdso_offset_clock_gettime 0x0420\n"; got != want {
		t.Fatalf("generateVDSOOffsets() = %q, want %q", got, want)
	}
}

func TestGenerateStackProtectorFlags(t *testing.T) {
	out, err := generateStackProtectorFlags(map[string]string{"CONFIG_STACKPROTECTOR_PER_TASK": "y"}, []byte("#define TSK_STACK_CANARY 1432 /* offsetof(struct task_struct, stack_canary) */\n"))
	if err != nil {
		t.Fatalf("generateStackProtectorFlags() failed: %v", err)
	}
	want := "-mstack-protector-guard=sysreg\n-mstack-protector-guard-reg=sp_el0\n-mstack-protector-guard-offset=1432\n"
	if got := string(out); got != want {
		t.Fatalf("generateStackProtectorFlags() = %q, want %q", got, want)
	}
}

func TestGenerateStackProtectorFlagsDisabled(t *testing.T) {
	out, err := generateStackProtectorFlags(nil, []byte("#define TSK_STACK_CANARY 1432\n"))
	if err != nil {
		t.Fatalf("generateStackProtectorFlags() failed: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("generateStackProtectorFlags() = %q, want empty", string(out))
	}
}
