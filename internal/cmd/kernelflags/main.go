// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"

	"linux.bzl/internal/kconfig"
)

func main() {
	configPath := flag.String("config", "", "Resolved Linux .config file")
	arch := flag.String("arch", "x86", "Linux ARCH value")
	outPath := flag.String("out", "", "Output Clang response file")
	asmOutPath := flag.String("asm_out", "", "Output assembler response file")
	flag.Parse()

	if *configPath == "" || *outPath == "" {
		flag.PrintDefaults()
		os.Exit(2)
	}

	file, err := os.Open(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open config: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()

	config, err := kconfig.ParseConfig(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse config: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(*outPath, []byte(responseFile(linuxCFlags(config, *arch))), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write flags: %v\n", err)
		os.Exit(1)
	}
	if *asmOutPath != "" {
		if err := os.WriteFile(*asmOutPath, []byte(responseFile(linuxAFlags(config, *arch))), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write assembler flags: %v\n", err)
			os.Exit(1)
		}
	}
}

func linuxCFlags(config map[string]string, arch string) []string {
	flags := []string{
		"-std=gnu11",
		"-fshort-wchar",
		"-funsigned-char",
		"-fno-common",
		"-fno-PIE",
		"-fno-strict-aliasing",
		"-fno-delete-null-pointer-checks",
		"-Wno-address-of-packed-member",
		"-Wno-frame-address",
		"-Wno-format-security",
		"-Wno-override-init",
		"-Wno-pointer-sign",
		"-Wno-trigraphs",
	}
	if enabled(config, "CONFIG_CC_IS_CLANG") {
		flags = append(flags,
			"-Wno-default-const-init-unsafe",
			"-Wno-default-const-init-var-unsafe",
			"-Wno-gnu",
			"-Wno-gnu-variable-sized-type-not-at-end",
			"-Wno-initializer-overrides",
		)
	}
	if enabled(config, "CONFIG_CC_OPTIMIZE_FOR_SIZE") {
		flags = append(flags, "-Os")
	} else {
		flags = append(flags, "-O2")
	}
	if enabled(config, "CONFIG_READABLE_ASM") {
		flags = append(flags, "-fno-reorder-blocks", "-fno-ipa-cp-clone", "-fno-partial-inlining")
	}
	switch {
	case enabled(config, "CONFIG_STACKPROTECTOR_STRONG"):
		flags = append(flags, "-fstack-protector-strong")
	case enabled(config, "CONFIG_STACKPROTECTOR"):
		flags = append(flags, "-fstack-protector")
	default:
		flags = append(flags, "-fno-stack-protector")
	}
	if enabled(config, "CONFIG_FRAME_POINTER") {
		flags = append(flags, "-fno-omit-frame-pointer", "-fno-optimize-sibling-calls")
	} else if !enabled(config, "CONFIG_FUNCTION_TRACER") {
		flags = append(flags, "-fomit-frame-pointer")
	}
	if enabled(config, "CONFIG_INIT_STACK_ALL_PATTERN") {
		flags = append(flags, "-ftrivial-auto-var-init=pattern")
	}
	if enabled(config, "CONFIG_INIT_STACK_ALL_ZERO") {
		flags = append(flags, "-ftrivial-auto-var-init=zero")
		if enabled(config, "CONFIG_CC_HAS_AUTO_VAR_INIT_ZERO_ENABLER") {
			flags = append(flags, "-enable-trivial-auto-var-init-zero-knowing-it-will-be-removed-from-clang")
		}
	}
	if enabled(config, "CONFIG_ZERO_CALL_USED_REGS") {
		flags = append(flags, "-fzero-call-used-regs=used-gpr")
	}
	flags = append(flags, ftraceFlags(config)...)
	if enabled(config, "CONFIG_DEBUG_SECTION_MISMATCH") {
		flags = append(flags, "-fno-inline-functions-called-once")
	}
	if alignment := config["CONFIG_FUNCTION_ALIGNMENT"]; alignment != "" {
		if enabled(config, "CONFIG_CC_HAS_MIN_FUNCTION_ALIGNMENT") && !enabled(config, "CONFIG_CC_IS_CLANG") {
			flags = append(flags, "-fmin-function-alignment="+alignment)
		} else {
			flags = append(flags, "-falign-functions="+alignment)
		}
	}
	flags = append(flags,
		"-fno-strict-overflow",
		"-fno-stack-check",
		"-fno-builtin-wcslen",
	)
	if !enabled(config, "CONFIG_CC_IS_CLANG") {
		flags = append(flags, "-fconserve-stack")
	}
	if arch == "x86" {
		flags = append(flags, x86CFlags(config)...)
	}
	if arch == "arm64" {
		flags = append(flags, arm64CFlags(config)...)
	}
	return flags
}

func ftraceFlags(config map[string]string) []string {
	if !enabled(config, "CONFIG_FUNCTION_TRACER") {
		return nil
	}
	flags := []string{}
	if enabled(config, "CONFIG_FTRACE_MCOUNT_USE_CC") {
		if !enabled(config, "CONFIG_CC_IS_CLANG") {
			flags = append(flags, "-mrecord-mcount")
		}
		if enabled(config, "CONFIG_HAVE_NOP_MCOUNT") {
			flags = append(flags, "-mnop-mcount")
		}
	}
	if enabled(config, "CONFIG_HAVE_FENTRY") {
		flags = append(flags, "-mfentry")
	}
	flags = append(flags, ftraceUsingFlags(config)...)
	return flags
}

func ftraceUsingFlags(config map[string]string) []string {
	if !enabled(config, "CONFIG_FUNCTION_TRACER") {
		return nil
	}
	flags := []string{}
	if enabled(config, "CONFIG_FTRACE_MCOUNT_USE_CC") && enabled(config, "CONFIG_HAVE_NOP_MCOUNT") {
		flags = append(flags, "-DCC_USING_NOP_MCOUNT")
	}
	if enabled(config, "CONFIG_FTRACE_MCOUNT_USE_OBJTOOL") && enabled(config, "CONFIG_HAVE_OBJTOOL_NOP_MCOUNT") {
		flags = append(flags, "-DCC_USING_NOP_MCOUNT")
	}
	if enabled(config, "CONFIG_HAVE_FENTRY") {
		flags = append(flags, "-DCC_USING_FENTRY")
	}
	return flags
}

func x86CFlags(config map[string]string) []string {
	flags := []string{
		"-mno-sse",
		"-mno-mmx",
		"-mno-sse2",
		"-mno-3dnow",
		"-mno-avx",
		"-mno-sse4a",
	}
	if enabled(config, "CONFIG_X86_KERNEL_IBT") {
		flags = append(flags, "-fcf-protection=branch", "-fno-jump-tables")
	} else {
		flags = append(flags, "-fcf-protection=none")
	}
	if enabled(config, "CONFIG_X86_64") {
		flags = append(flags,
			"-m64",
			"-falign-jumps=1",
			"-falign-loops=1",
			"-mno-80387",
			"-mno-fp-ret-in-387",
			"-mstack-alignment=8",
			"-mskip-rax-setup",
			"-mno-red-zone",
			"-mcmodel=kernel",
		)
		if enabled(config, "CONFIG_X86_NATIVE_CPU") {
			flags = append(flags, "-march=native")
		} else {
			flags = append(flags, "-march=x86-64", "-mtune=generic")
		}
		if enabled(config, "CONFIG_STACKPROTECTOR") && enabled(config, "CONFIG_SMP") {
			flags = append(flags,
				"-mstack-protector-guard-reg=gs",
				"-mstack-protector-guard-symbol=__ref_stack_chk_guard",
			)
		}
	} else {
		flags = append(flags, "-m32", "-msoft-float", "-mregparm=3", "-freg-struct-return", "-fno-pic")
		if enabled(config, "CONFIG_STACKPROTECTOR") && enabled(config, "CONFIG_SMP") {
			flags = append(flags,
				"-mstack-protector-guard-reg=fs",
				"-mstack-protector-guard-symbol=__ref_stack_chk_guard",
			)
		}
	}
	flags = append(flags, "-Wno-sign-compare", "-fno-asynchronous-unwind-tables")
	if enabled(config, "CONFIG_MITIGATION_RETPOLINE") {
		if enabled(config, "CONFIG_CC_IS_CLANG") {
			flags = append(flags, "-mretpoline-external-thunk")
		} else {
			flags = append(flags, "-mindirect-branch=thunk-extern", "-mindirect-branch-register")
		}
	}
	if enabled(config, "CONFIG_MITIGATION_RETHUNK") {
		flags = append(flags, "-mfunction-return=thunk-extern")
	}
	if enabled(config, "CONFIG_MITIGATION_SLS") {
		flags = append(flags, "-mharden-sls=all")
	}
	if enabled(config, "CONFIG_CALL_PADDING") {
		padding := config["CONFIG_FUNCTION_PADDING_BYTES"]
		if padding != "" {
			flags = append(flags, "-fpatchable-function-entry="+padding+","+padding)
		}
	}
	return flags
}

func linuxAFlags(config map[string]string, arch string) []string {
	switch arch {
	case "arm64":
		return arm64AFlags(config)
	case "x86":
		return append(x86CFlags(config), ftraceUsingFlags(config)...)
	default:
		return nil
	}
}

func arm64CFlags(config map[string]string) []string {
	flags := []string{
		"-mgeneral-regs-only",
		"-Wno-psabi",
	}
	if enabled(config, "CONFIG_CPU_BIG_ENDIAN") {
		flags = append(flags, "-mbig-endian")
	} else {
		flags = append(flags, "-mlittle-endian")
	}
	if enabled(config, "CONFIG_UNWIND_TABLES") {
		flags = append(flags, "-fasynchronous-unwind-tables")
	} else {
		flags = append(flags, "-fno-asynchronous-unwind-tables", "-fno-unwind-tables")
	}
	if enabled(config, "CONFIG_ARM64_BTI_KERNEL") {
		flags = append(flags, "-mbranch-protection=pac-ret+bti")
	} else if enabled(config, "CONFIG_ARM64_PTR_AUTH_KERNEL") {
		flags = append(flags, "-mbranch-protection=pac-ret")
	} else {
		flags = append(flags, "-mbranch-protection=none")
	}
	asmArch := "armv8.4-a"
	if enabled(config, "CONFIG_AS_HAS_ARMV8_5") {
		asmArch = "armv8.5-a"
	}
	flags = append(flags, "-Wa,-march="+asmArch, "-DARM64_ASM_ARCH=\\\""+asmArch+"\\\"")
	if enabled(config, "CONFIG_SHADOW_CALL_STACK") {
		flags = append(flags, "-ffixed-x18")
	}
	if enabled(config, "CONFIG_DYNAMIC_FTRACE_WITH_CALL_OPS") {
		flags = append(flags, "-DCC_USING_PATCHABLE_FUNCTION_ENTRY", "-fpatchable-function-entry=4,2")
	} else if enabled(config, "CONFIG_DYNAMIC_FTRACE_WITH_ARGS") {
		flags = append(flags, "-DCC_USING_PATCHABLE_FUNCTION_ENTRY", "-fpatchable-function-entry=2")
	}
	kasanShift := ""
	if enabled(config, "CONFIG_KASAN_SW_TAGS") {
		kasanShift = "4"
	} else if enabled(config, "CONFIG_KASAN_GENERIC") {
		kasanShift = "3"
	}
	if kasanShift != "" {
		flags = append(flags, "-DKASAN_SHADOW_SCALE_SHIFT="+kasanShift)
	}
	return flags
}

func arm64AFlags(config map[string]string) []string {
	flags := []string{
		"-D__ASSEMBLY__",
		"-Wno-psabi",
	}
	if enabled(config, "CONFIG_CPU_BIG_ENDIAN") {
		flags = append(flags, "-mbig-endian")
	} else {
		flags = append(flags, "-mlittle-endian")
	}
	asmArch := "armv8.4-a"
	if enabled(config, "CONFIG_AS_HAS_ARMV8_5") {
		asmArch = "armv8.5-a"
	}
	flags = append(flags, "-Wa,-march="+asmArch, "-DARM64_ASM_ARCH=\\\""+asmArch+"\\\"")
	if enabled(config, "CONFIG_SHADOW_CALL_STACK") {
		flags = append(flags, "-ffixed-x18")
	}
	return flags
}

func enabled(config map[string]string, key string) bool {
	value := config[key]
	return value != "" && value != "n"
}

func responseFile(flags []string) string {
	out := ""
	for _, flag := range flags {
		out += flag + "\n"
	}
	return out
}
