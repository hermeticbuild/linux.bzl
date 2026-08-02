package kconfig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLinuxProbeShell(
	t *testing.T,
	architecture string,
) func(context.Context, string) (string, error) {
	t.Helper()
	shell, err := LinuxProbeShell(
		architecture,
		LinuxProbeDefaultRustcVersion,
		LinuxProbeDefaultRustcLLVMVersion,
	)
	if err != nil {
		t.Fatalf("LinuxProbeShell() failed: %v", err)
	}
	return shell
}

func TestLinuxLLVMProbePolicy(t *testing.T) {
	shell := testLinuxProbeShell(t, "x86_64")
	for _, test := range []struct {
		command string
		want    string
	}{
		{command: "/src/scripts/cc-version.sh clang", want: "Clang 220108"},
		{command: "clang --version", want: "clang version 22.1.8None"},
		{command: "/src/scripts/as-version.sh clang -fintegrated-as", want: "LLVM 0"},
		{command: "/src/scripts/ld-version.sh ld.lld", want: "LLD 220108"},
		{command: "/src/scripts/pahole-version.sh pahole", want: "131"},
		{command: "bindgen --version workaround-for-0.69.0 2>/dev/null", want: "bindgen 0.72.1"},
		{command: "/src/scripts/rustc-version.sh rustc", want: "109700"},
		{command: "/src/scripts/rustc-llvm-version.sh rustc", want: "220106"},
	} {
		got, err := shell(context.Background(), test.command)
		if err != nil {
			t.Fatalf("shell(%q) failed: %v", test.command, err)
		}
		if got != test.want {
			t.Fatalf("shell(%q) = %q, want %q", test.command, got, test.want)
		}
	}
}

func TestLinuxLLVMProbeShellSupportsKconfigIncludeProbes(t *testing.T) {
	rootDir := t.TempDir()
	scriptsDir := filepath.Join(rootDir, "scripts")
	if err := os.Mkdir(scriptsDir, 0o755); err != nil {
		t.Fatalf("Mkdir() failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "Kconfig.include"), []byte(`
comma := ,
if-success = $(shell,{ $(1); } >/dev/null 2>&1 && echo "$(2)" || echo "$(3)")
success = $(if-success,$(1),y,n)
failure = $(if-success,$(1),n,y)

$(error-if,$(failure,command -v $(CC)),C compiler '$(CC)' not found)
$(error-if,$(failure,command -v $(LD)),linker '$(LD)' not found)

cc-info := $(shell,$(srctree)/scripts/cc-version.sh $(CC))
$(error-if,$(success,test -z "$(cc-info)"),Sorry$(comma) this C compiler is not supported.)
cc-name := $(shell,set -- $(cc-info) && echo $1)
cc-version := $(shell,set -- $(cc-info) && echo $2)

as-info := $(shell,$(srctree)/scripts/as-version.sh $(CC) $(CLANG_FLAGS))
$(error-if,$(success,test -z "$(as-info)"),Sorry$(comma) this assembler is not supported.)
as-name := $(shell,set -- $(as-info) && echo $1)
as-version := $(shell,set -- $(as-info) && echo $2)

ld-info := $(shell,$(srctree)/scripts/ld-version.sh $(LD))
$(error-if,$(success,test -z "$(ld-info)"),Sorry$(comma) this linker is not supported.)
ld-name := $(shell,set -- $(ld-info) && echo $1)
ld-version := $(shell,set -- $(ld-info) && echo $2)

config CC_IS_GCC
	def_bool $(success,test "$(cc-name)" = GCC)

config CC_IS_CLANG
	def_bool $(success,test "$(cc-name)" = Clang)
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}

	shell := testLinuxProbeShell(t, "x86_64")
	tree, err := Parse(context.Background(), strings.NewReader(`
srctree := `+rootDir+`
CC := clang
LD := ld.lld
CLANG_FLAGS := -fintegrated-as

source "scripts/Kconfig.include"

config CC_IS_CLANG_TEST
	bool
	default y if CC_IS_CLANG

config CC_IS_GCC_TEST
	bool
	default y if CC_IS_GCC

config LLD_MAJOR
	int
	default $(shell,expr $(ld-version) / 10000)

config PAHOLE_VERSION
	int
	default $(shell,$(srctree)/scripts/pahole-version.sh $(PAHOLE))

config BINDGEN_VERSION_TEXT
	string
	default "$(shell,$(BINDGEN) --version workaround-for-0.69.0 2>/dev/null)"
`), "Kconfig", Options{
		AllowShell: true,
		RootDir:    rootDir,
		Env: map[string]string{
			"BINDGEN": "bindgen",
			"PAHOLE":  "pahole",
		},
		Shell: shell,
	})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	resolved, err := tree.ResolveConfig("test", nil)
	if err != nil {
		t.Fatalf("ResolveConfig() failed: %v", err)
	}
	if got := resolved.Value("CONFIG_CC_IS_CLANG"); got != "y" {
		t.Fatalf("CONFIG_CC_IS_CLANG = %q, want y", got)
	}
	if got := resolved.Value("CONFIG_CC_IS_GCC"); got != "n" {
		t.Fatalf("CONFIG_CC_IS_GCC = %q, want n", got)
	}
	if got := resolved.Value("CONFIG_CC_IS_CLANG_TEST"); got != "y" {
		t.Fatalf("CONFIG_CC_IS_CLANG_TEST = %q, want y", got)
	}
	if got := resolved.Value("CONFIG_CC_IS_GCC_TEST"); got != "n" {
		t.Fatalf("CONFIG_CC_IS_GCC_TEST = %q, want n", got)
	}
	if got := resolved.Value("CONFIG_LLD_MAJOR"); got != "22" {
		t.Fatalf("CONFIG_LLD_MAJOR = %q, want 22", got)
	}
	if got := resolved.Value("CONFIG_PAHOLE_VERSION"); got != "131" {
		t.Fatalf("CONFIG_PAHOLE_VERSION = %q, want 131", got)
	}
	if got := resolved.Value("CONFIG_BINDGEN_VERSION_TEXT"); got != "\"bindgen 0.72.1\"" {
		t.Fatalf("CONFIG_BINDGEN_VERSION_TEXT = %q, want bindgen 0.72.1", got)
	}
}

func TestLinuxLLVMProbeShellSupportsLLVMNmAndArProbes(t *testing.T) {
	shell := testLinuxProbeShell(t, "x86_64")
	for _, command := range []string{
		"llvm-nm --help | head -n 1 | grep -qi llvm",
		"llvm-ar --help | head -n 1 | grep -qi llvm",
		`ld.lld -v --gc-sections`,
		`printf "%b\n" ".arch_extension lse" | clang -fintegrated-as -Wa,--fatal-warnings -c -x assembler-with-cpp -o /dev/null -`,
		`echo 'int __seg_fs fs; int __seg_gs gs;' | clang -x c - -S -o /dev/null`,
	} {
		out, err := shell(context.Background(), "{ "+command+"; } >/dev/null 2>&1 && echo \"y\" || echo \"n\"")
		if err != nil {
			t.Fatalf("shell(%q) failed: %v", command, err)
		}
		if got := strings.TrimSpace(out); got != "y" {
			t.Fatalf("shell(%q) = %q, want y", command, got)
		}
	}
}

func TestLinuxLLVMProbeShellSupportsCCOptionInt128Probe(t *testing.T) {
	rootDir := t.TempDir()
	scriptsDir := filepath.Join(rootDir, "scripts")
	if err := os.Mkdir(scriptsDir, 0o755); err != nil {
		t.Fatalf("Mkdir() failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "Kconfig.include"), []byte(`
if-success = $(shell,{ $(1); } >/dev/null 2>&1 && echo "$(2)" || echo "$(3)")
success = $(if-success,$(1),y,n)
cc-option = $(success,trap "rm -rf .tmp_$$" EXIT; mkdir .tmp_$$; $(CC) -Werror $(CLANG_FLAGS) $(1) -c -x c /dev/null -o .tmp_$$/tmp.o)
cc-option-bit = $(if-success,$(CC) -Werror $(1) -E -x c /dev/null -o /dev/null,$(1))
m64-flag := $(cc-option-bit,-m64)
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}

	var commands []string
	baseShell := testLinuxProbeShell(t, "x86_64")
	shell := func(ctx context.Context, command string) (string, error) {
		commands = append(commands, command)
		return baseShell(ctx, command)
	}
	tree, err := Parse(context.Background(), strings.NewReader(`
srctree := `+rootDir+`
CC := clang
CLANG_FLAGS := -fintegrated-as

source "scripts/Kconfig.include"

config 64BIT
	def_bool y

config CC_HAS_INT128
	def_bool !$(cc-option,$(m64-flag) -D__SIZEOF_INT128__=0) && 64BIT
`), "Kconfig", Options{
		AllowShell: true,
		RootDir:    rootDir,
		Shell:      shell,
	})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	resolved, err := tree.ResolveConfig("test", nil)
	if err != nil {
		t.Fatalf("ResolveConfig() failed: %v", err)
	}
	if got := resolved.Value("CONFIG_CC_HAS_INT128"); got != "y" {
		t.Fatalf("CONFIG_CC_HAS_INT128 = %q, want y; commands=%q", got, commands)
	}
}

func TestLinuxLLVMProbeShellHandlesCanonicalM32PreprocessorProbeByProfile(t *testing.T) {
	const command = `{ clang -Werror -m32 -E -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`
	for profile, want := range map[string]string{
		"x86_64":  "y",
		"aarch64": "n",
		"armv7":   "y",
		"riscv64": "y",
		"ppc64le": "y",
	} {
		t.Run(profile, func(t *testing.T) {
			shell := testLinuxProbeShell(t, profile)
			got, err := shell(context.Background(), command)
			if err != nil || got != want {
				t.Fatalf("shell(%q) = %q, %v; want %q", command, got, err, want)
			}
		})
	}
}

func TestLinuxLLVMProbeShellHandlesCanonicalM64PreprocessorProbeByProfile(t *testing.T) {
	const command = `{ clang -Werror -m64 -E -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`
	for profile, want := range map[string]string{
		"x86_64":  "y",
		"aarch64": "y",
		"armv7":   "n",
		"riscv64": "y",
		"ppc64le": "y",
	} {
		t.Run(profile, func(t *testing.T) {
			shell := testLinuxProbeShell(t, profile)
			got, err := shell(context.Background(), command)
			if err != nil || got != want {
				t.Fatalf("shell(%q) = %q, %v; want %q", command, got, err, want)
			}
		})
	}
}

func TestLinuxLLVMProbeShellHandlesWrappedPatchableEntryProbeByProfile(t *testing.T) {
	const command = `{ trap "rm -rf .tmp_$$" EXIT; mkdir .tmp_$$; clang -Werror -fintegrated-as -fpatchable-function-entry=8 -c -x c /dev/null -o .tmp_$$/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`
	for profile, want := range map[string]string{
		"x86_64":  "y",
		"aarch64": "y",
		"armv7":   "n",
		"riscv64": "y",
		"ppc64le": "y",
	} {
		t.Run(profile, func(t *testing.T) {
			shell := testLinuxProbeShell(t, profile)
			got, err := shell(context.Background(), command)
			if err != nil || got != want {
				t.Fatalf("shell(%q) = %q, %v; want %q", command, got, err, want)
			}
		})
	}
}

func TestLinuxLLVMProbeShellHandlesGroupedARMStackGuardProbeByProfile(t *testing.T) {
	const command = `{ trap "rm -rf .tmp_$$" EXIT; mkdir .tmp_$$; clang -Werror -fintegrated-as -mtp=cp15 -mstack-protector-guard=tls -mstack-protector-guard-offset=0 -c -x c /dev/null -o .tmp_$$/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`
	for profile, want := range map[string]string{
		"x86_64":  "n",
		"aarch64": "n",
		"armv7":   "y",
		"riscv64": "n",
		"ppc64le": "n",
	} {
		t.Run(profile, func(t *testing.T) {
			shell := testLinuxProbeShell(t, profile)
			got, err := shell(context.Background(), command)
			if err != nil || got != want {
				t.Fatalf("shell(%q) = %q, %v; want %q", command, got, err, want)
			}
		})
	}
}

func TestLinuxLLVMProbeShellRejectsExactARMV7KMSANCandidates(t *testing.T) {
	shell := testLinuxProbeShell(t, "armv7")
	for _, candidate := range []string{
		"-fsanitize=kernel-memory",
		"-fsanitize=kernel-memory -fsanitize-memory-param-retval",
		"-fsanitize=kernel-memory -mllvm -msan-disable-checks=1",
	} {
		command := `{ clang -Werror -fintegrated-as ` + candidate + ` -c -x c /dev/null -o .tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`
		got, err := shell(context.Background(), command)
		if err != nil || got != "n" {
			t.Errorf("shell(%q) = %q, %v; want n", command, got, err)
		}
	}
}

func TestLinuxLLVMProbeShellSupportsExactRISCVKconfigCandidates(t *testing.T) {
	shell := testLinuxProbeShell(t, "riscv64")
	for _, candidate := range []string{
		"-fsanitize=shadow-call-stack",
		"-mabi=lp64 -march=rv64imv",
		"-mabi=ilp32 -march=rv32imv",
		"-mabi=lp64 -march=rv64ima_zabha",
		"-mabi=ilp32 -march=rv32ima_zabha",
		"-mabi=lp64 -march=rv64ima_zacas",
		"-mabi=ilp32 -march=rv32ima_zacas",
		"-mabi=lp64 -march=rv64ima_zbb",
		"-mabi=ilp32 -march=rv32ima_zbb",
		"-mabi=lp64 -march=rv64ima_zba",
		"-mabi=ilp32 -march=rv32ima_zba",
		"-mabi=lp64 -march=rv64ima_zbc",
		"-mabi=ilp32 -march=rv32ima_zbc",
		"-mabi=lp64 -march=rv64ima_zbkb",
		"-mabi=ilp32 -march=rv32ima_zbkb",
		"-mstack-protector-guard=tls -mstack-protector-guard-reg=tp -mstack-protector-guard-offset=0",
	} {
		command := `{ clang -Werror -fintegrated-as ` + candidate + ` -c -x c /dev/null -o .tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`
		got, err := shell(context.Background(), command)
		if err != nil || got != "y" {
			t.Errorf("shell(%q) = %q, %v; want y", command, got, err)
		}
	}
	for _, candidate := range []string{
		"-fsanitize=kernel-memory",
		"-fsanitize=kernel-memory -fsanitize-memory-param-retval",
		"-fsanitize=kernel-memory -mllvm -msan-disable-checks=1",
	} {
		command := `{ clang -Werror -fintegrated-as ` + candidate + ` -c -x c /dev/null -o .tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`
		got, err := shell(context.Background(), command)
		if err != nil || got != "n" {
			t.Errorf("shell(%q) = %q, %v; want n", command, got, err)
		}
	}
	const linkerCommand = `ld.lld -v --no-relax-gp`
	got, err := shell(context.Background(), `{ `+linkerCommand+`; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	if err != nil || got != "y" {
		t.Errorf("shell(%q) = %q, %v; want y", linkerCommand, got, err)
	}
}

func TestLinuxLLVMProbeShellSupportsExactRISCVAssemblerCandidates(t *testing.T) {
	shell := testLinuxProbeShell(t, "riscv64")
	for _, source := range []string{
		`.insn 0x100000f`,
		`.option arch, +m`,
		`.option arch, +v, +zvkb`,
		`.reloc label, R_RISCV_SET_ULEB128, 127\n.reloc label, R_RISCV_SUB_ULEB128, 127\nlabel:\n.word 0`,
	} {
		command := `printf "%b\n" "` + source + `" | clang -fintegrated-as -Wa,--fatal-warnings -c -x assembler-with-cpp -o /dev/null -`
		got, err := shell(context.Background(), `{ `+command+`; } >/dev/null 2>&1 && echo "y" || echo "n"`)
		if err != nil || got != "y" {
			t.Errorf("shell(%q) = %q, %v; want y", command, got, err)
		}
	}
}

func TestLinuxLLVMProbeShellSupportsExactPPC64LEKconfigCandidates(t *testing.T) {
	shell := testLinuxProbeShell(t, "ppc64le")
	for _, candidate := range []string{
		"-mabi=elfv2",
		"-mcpu=power10 -mprefixed",
		"-mcpu=power10 -mpcrel",
		"-fpatchable-function-entry=2",
		"-mtune=power10",
		"-mtune=power9",
		"-mtune=power8",
		"-fsanitize=kernel-memory",
		"-fsanitize=kernel-memory -fsanitize-memory-param-retval",
		"-fsanitize=kernel-memory -mllvm -msan-disable-checks=1",
		"-m64 -mstack-protector-guard=tls -mstack-protector-guard-reg=r13 -mstack-protector-guard-offset=0",
	} {
		command := `{ clang -Werror -fintegrated-as ` + candidate + ` -c -x c /dev/null -o .tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`
		got, err := shell(context.Background(), command)
		if err != nil || got != "y" {
			t.Errorf("shell(%q) = %q, %v; want y", command, got, err)
		}
	}
	const unsupportedPPC32Guard = "-m32 -mstack-protector-guard=tls -mstack-protector-guard-reg=r2 -mstack-protector-guard-offset=0"
	command := `{ clang -Werror -fintegrated-as ` + unsupportedPPC32Guard + ` -c -x c /dev/null -o .tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`
	got, err := shell(context.Background(), command)
	if err != nil || got != "n" {
		t.Errorf("shell(%q) = %q, %v; want n", command, got, err)
	}
}

func TestLinuxProbeShellKeepsCompilerAndHostFactsFixed(t *testing.T) {
	shell, err := LinuxProbeShell("x86_64", 109900, 230001)
	if err != nil {
		t.Fatalf("LinuxProbeShell() failed: %v", err)
	}
	for _, test := range []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "linker version",
			command: "/src/scripts/ld-version.sh ld.lld",
			want:    "LLD 220108",
		},
		{
			name:    "pahole version",
			command: "/src/scripts/pahole-version.sh pahole",
			want:    "131",
		},
		{
			name:    "bindgen version",
			command: "bindgen --version workaround-for-0.69.0 2>/dev/null",
			want:    "bindgen 0.72.1",
		},
		{
			name:    "selected rustc version",
			command: "/src/scripts/rustc-version.sh rustc",
			want:    "109900",
		},
		{
			name:    "selected Rust LLVM version",
			command: "/src/scripts/rustc-llvm-version.sh rustc",
			want:    "230001",
		},
		{
			name:    "generic compiler option",
			command: `{ clang -Werror -fno-stack-protector -c -x c /dev/null -o /tmp/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			want:    "y",
		},
		{
			name:    "compiler smoke test",
			command: `{ echo 'void foo(void) { asm inline (""); }' | clang -x c - -c -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			want:    "y",
		},
		{
			name:    "compiler option with trailing Werror",
			command: `{ echo '__attribute__((no_profile_instrument_function)) int x();' | clang -x c - -c -o /dev/null -Werror; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			want:    "y",
		},
		{
			name:    "builtin macro redefined",
			command: `{ clang -Werror -D__SIZEOF_INT128__=0 -c -x c /dev/null -o /tmp/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			want:    "n",
		},
		{
			name:    "builtin macro redefined to same value",
			command: `{ clang -Werror -D__SIZEOF_INT128__=16 -c -x c /dev/null -o /tmp/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			want:    "y",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := shell(context.Background(), test.command)
			if err != nil {
				t.Fatalf("shell(%q) failed: %v", test.command, err)
			}
			if got != test.want {
				t.Fatalf("shell(%q) = %q, want %q", test.command, got, test.want)
			}
		})
	}
}

func TestLinuxLLVMProbeShellUsesArchitectureCapabilityBaseline(t *testing.T) {
	command := `{ clang -Werror -fintegrated-as -fsanitize=kernel-memory -c -x c /dev/null -o /tmp/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`
	for _, test := range []struct {
		architecture string
		want         string
	}{
		{architecture: "x86_64", want: "y"},
		{architecture: "arm64", want: "n"},
	} {
		t.Run(test.architecture, func(t *testing.T) {
			shell := testLinuxProbeShell(t, test.architecture)
			got, err := shell(context.Background(), command)
			if err != nil {
				t.Fatalf("shell(%q) failed: %v", command, err)
			}
			if got != test.want {
				t.Fatalf("shell(%q) = %q, want %q", command, got, test.want)
			}
		})
	}
}

func TestLinuxLLVMProbeShellRejectsUnknownCompilerCandidate(t *testing.T) {
	shell := testLinuxProbeShell(t, "x86_64")
	command := `{ clang -Werror -fbrand-new-kernel-flag -c -x c /dev/null -o /tmp/tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`
	_, err := shell(context.Background(), command)
	if err == nil {
		t.Fatalf("shell(%q) unexpectedly succeeded", command)
	}
	for _, want := range []string{"-fbrand-new-kernel-flag", "x86_64", "Clang 22.1.8"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("shell(%q) error %q does not contain %q", command, err, want)
		}
	}
}

func TestLinuxLLVMProbeShellRejectsUnknownProbeCommand(t *testing.T) {
	shell := testLinuxProbeShell(t, "x86_64")
	command := `{ custom-kernel-probe --answer; } >/dev/null 2>&1 && echo "y" || echo "n"`
	_, err := shell(context.Background(), command)
	if err == nil || !strings.Contains(err.Error(), "custom-kernel-probe --answer") {
		t.Fatalf("shell(%q) error = %v, want contextual unsupported-probe error", command, err)
	}
}

func TestLinuxLLVMProbeShellKeepsRustFactsDynamic(t *testing.T) {
	shell, err := LinuxProbeShell("arm64", 109900, 230001)
	if err != nil {
		t.Fatalf("LinuxProbeShell() failed: %v", err)
	}
	for _, test := range []struct {
		command string
		want    string
	}{
		{command: "/src/scripts/rustc-version.sh rustc", want: "109900"},
		{command: "/src/scripts/rustc-llvm-version.sh rustc", want: "230001"},
	} {
		got, err := shell(context.Background(), test.command)
		if err != nil {
			t.Fatalf("shell(%q) failed: %v", test.command, err)
		}
		if got != test.want {
			t.Fatalf("shell(%q) = %q, want %q", test.command, got, test.want)
		}
	}
}
