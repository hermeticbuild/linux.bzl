package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

type configPayloadManifest struct {
	Arch     string            `json:"arch"`
	Payloads map[string]string `json:"payloads"`
	Version  string            `json:"version"`
}

func main() {
	configPath := flag.String("config", "", "Resolved Linux .config file")
	arch := flag.String("arch", "x86", "Linux ARCH value")
	version := flag.String("version", "", "Linux kernel version")
	outPath := flag.String("out", "", "Output Clang response file")
	asmOutPath := flag.String("asm_out", "", "Output assembler response file")
	batchManifestPath := flag.String("batch_manifest", "", "JSON manifest of content-addressed config payloads")
	batchOutDir := flag.String("batch_out_dir", "", "Output root for content-addressed config payloads")
	flag.Parse()

	if *batchManifestPath != "" || *batchOutDir != "" {
		if *batchManifestPath == "" || *batchOutDir == "" || *configPath != "" || *outPath != "" || *asmOutPath != "" {
			flag.PrintDefaults()
			os.Exit(2)
		}
		if err := materializeConfigPayloads(*batchManifestPath, *batchOutDir); err != nil {
			fmt.Fprintf(os.Stderr, "materialize config payloads: %v\n", err)
			os.Exit(1)
		}
		return
	}

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

	if err := os.WriteFile(*outPath, []byte(responseFile(linuxCFlags(config, *arch, *version))), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write flags: %v\n", err)
		os.Exit(1)
	}
	if *asmOutPath != "" {
		if err := os.WriteFile(*asmOutPath, []byte(responseFile(linuxAFlags(config, *arch, *version))), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write assembler flags: %v\n", err)
			os.Exit(1)
		}
	}
}

func materializeConfigPayloads(manifestPath, outDir string) error {
	file, err := os.Open(manifestPath)
	if err != nil {
		return fmt.Errorf("open manifest: %w", err)
	}
	defer file.Close()

	var manifest configPayloadManifest
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode manifest: trailing JSON value")
		}
		return fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.Payloads == nil {
		return fmt.Errorf("manifest payloads must be an object")
	}
	supportedArchitectures := map[string]bool{
		"arm":     true,
		"arm64":   true,
		"powerpc": true,
		"riscv":   true,
		"x86":     true,
	}
	if manifest.Arch != "" && !supportedArchitectures[manifest.Arch] {
		return fmt.Errorf("unsupported Linux ARCH %q", manifest.Arch)
	}

	ids := make([]string, 0, len(manifest.Payloads))
	for id := range manifest.Payloads {
		if !isContentID(id) {
			return fmt.Errorf("invalid payload ID %q", id)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		config, err := parseConfigPayload(manifest.Payloads[id])
		if err != nil {
			return fmt.Errorf("parse payload %s: %w", id, err)
		}
		if err := materializeConfigPayload(outDir, id, config, manifest.Arch, manifest.Version); err != nil {
			return fmt.Errorf("materialize payload %s: %w", id, err)
		}
	}
	return nil
}

func parseConfigPayload(content string) (map[string]string, error) {
	config := map[string]string{}
	previousKey := ""
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		key := ""
		value := ""
		if strings.HasPrefix(line, "# CONFIG_") && strings.HasSuffix(line, " is not set") {
			key = strings.TrimSuffix(strings.TrimPrefix(line, "# "), " is not set")
			value = "n"
		} else {
			var found bool
			key, value, found = strings.Cut(line, "=")
			if !found {
				return nil, fmt.Errorf("invalid line %q", line)
			}
		}
		if !strings.HasPrefix(key, "CONFIG_") {
			return nil, fmt.Errorf("invalid key %q", key)
		}
		if _, ok := config[key]; ok {
			return nil, fmt.Errorf("repeated key %s", key)
		}
		if previousKey != "" && key < previousKey {
			return nil, fmt.Errorf("payload is not sorted: %s follows %s", key, previousKey)
		}
		config[key] = value
		previousKey = key
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan payload: %w", err)
	}
	return config, nil
}

func isContentID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func materializeConfigPayload(outDir, id string, config map[string]string, arch, version string) error {
	root := filepath.Join(outDir, id)
	autoConf := filepath.Join(root, "include", "config", "auto.conf")
	files := map[string]string{
		filepath.Join(root, ".config"): configText(config),
		autoConf:                       configText(config),
		filepath.Join(root, "include", "config", "auto.conf.cmd"):              "'cmd_" + filepath.ToSlash(autoConf) + " := bazel linux_config'\n",
		filepath.Join(root, "include", "config", "kernel.release"):             version + unquote(config["CONFIG_LOCALVERSION"]) + "\n",
		filepath.Join(root, "include", "generated", "autoconf.h"):              autoconfText(config),
		filepath.Join(root, "include", "generated", "integer-wrap.h"):          "",
		filepath.Join(root, "include", "generated", "rustc_cfg"):               rustcConfigText(config),
		filepath.Join(root, "include", "generated", "bazel_kbuild_aflags.rsp"): configFlagsResponse(config, arch, version, true),
		filepath.Join(root, "include", "generated", "bazel_kbuild_cflags.rsp"): configFlagsResponse(config, arch, version, false),
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(files[path]), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

func configText(config map[string]string) string {
	keys := sortedConfigKeys(config)
	var out strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&out, "%s=%s\n", key, config[key])
	}
	return out.String()
}

func autoconfText(config map[string]string) string {
	var out strings.Builder
	out.WriteString("/* Generated by Bazel linux_config. */\n")
	out.WriteString("#ifndef __GENERATED_AUTOCONF_H__\n")
	out.WriteString("#define __GENERATED_AUTOCONF_H__\n")
	for _, key := range sortedConfigKeys(config) {
		value := config[key]
		switch value {
		case "y":
			fmt.Fprintf(&out, "#define %s 1\n", key)
		case "m":
			fmt.Fprintf(&out, "#define %s_MODULE 1\n", key)
		case "n":
		default:
			fmt.Fprintf(&out, "#define %s %s\n", key, value)
		}
	}
	out.WriteString("#endif\n")
	return out.String()
}

func rustcConfigText(config map[string]string) string {
	var out strings.Builder
	for _, key := range sortedConfigKeys(config) {
		value := config[key]
		if value == "y" || value == "m" {
			fmt.Fprintf(&out, "--cfg=%s\n", key)
		}
		if value != "n" {
			rendered := value
			if !strings.HasPrefix(rendered, "\"") {
				rendered = "\"" + rendered + "\""
			}
			fmt.Fprintf(&out, "--cfg=%s=%s\n", key, rendered)
		}
	}
	return out.String()
}

func sortedConfigKeys(config map[string]string) []string {
	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func configFlagsResponse(config map[string]string, arch, version string, assembly bool) string {
	if arch == "" {
		return ""
	}
	if assembly {
		return responseFile(linuxAFlags(config, arch, version))
	}
	return responseFile(linuxCFlags(config, arch, version))
}

func unquote(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}

func linuxCFlags(config map[string]string, arch, version string) []string {
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
			"-fno-addrsig",
			"-Wno-default-const-init-unsafe",
			"-Wno-default-const-init-var-unsafe",
			"-Wno-gnu",
			"-Wno-gnu-variable-sized-type-not-at-end",
			"-Wno-initializer-overrides",
		)
		if enabled(config, "CONFIG_LTO_CLANG_THIN") {
			flags = append(flags, "-fno-lto", "-flto=thin", "-fsplit-lto-unit", "-fvisibility=hidden")
		} else if enabled(config, "CONFIG_LTO_CLANG_FULL") {
			flags = append(flags, "-fno-lto", "-flto", "-fvisibility=hidden")
		}
	}
	if enabled(config, "CONFIG_LD_DEAD_CODE_DATA_ELIMINATION") {
		flags = append(flags, "-ffunction-sections", "-fdata-sections")
	} else {
		flags = append(flags, "-fno-function-sections", "-fno-data-sections")
	}
	if enabled(config, "CONFIG_CC_OPTIMIZE_FOR_PERFORMANCE") {
		flags = append(flags, "-O2")
	} else if enabled(config, "CONFIG_CC_OPTIMIZE_FOR_SIZE") {
		flags = append(flags, "-Os")
	}
	if enabled(config, "CONFIG_READABLE_ASM") {
		flags = append(flags, "-fno-reorder-blocks", "-fno-ipa-cp-clone", "-fno-partial-inlining")
	}
	flags = append(flags, debugInfoFlags(config)...)
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
	flags = append(flags, ftraceFlags(config, arch)...)
	if enabled(config, "CONFIG_DEBUG_SECTION_MISMATCH") {
		flags = append(flags, "-fno-inline-functions-called-once")
	}
	if enabled(config, "CONFIG_CC_IS_CLANG") {
		flags = append(flags, "-fno-stack-clash-protection")
	}
	if alignment := config["CONFIG_FUNCTION_ALIGNMENT"]; alignment != "" {
		if enabled(config, "CONFIG_CC_HAS_MIN_FUNCTION_ALIGNMENT") && !enabled(config, "CONFIG_CC_IS_CLANG") {
			flags = append(flags, "-fmin-function-alignment="+alignment)
		} else {
			flags = append(flags, "-falign-functions="+alignment)
		}
	}
	if enabled(config, "CONFIG_CC_IS_CLANG") {
		flags = append(flags, "-fstrict-flex-arrays=3")
	}
	flags = append(flags,
		"-fno-strict-overflow",
		"-fno-stack-check",
		"-fno-builtin-wcslen",
	)
	if !enabled(config, "CONFIG_CC_IS_CLANG") {
		flags = append(flags, "-fconserve-stack")
	}
	switch arch {
	case "arm":
		flags = append(flags, armCFlags(config)...)
	case "arm64":
		flags = append(flags, arm64CFlags(config)...)
	case "powerpc":
		flags = append(flags, powerPCCFlags(config)...)
	case "riscv":
		flags = append(flags, riscvCFlags(config, version)...)
	case "x86":
		flags = append(flags, x86CFlags(config, version)...)
	}
	return flags
}

func ftraceFlags(config map[string]string, arch string) []string {
	if !enabled(config, "CONFIG_FUNCTION_TRACER") {
		return nil
	}
	flags := []string{}
	usesPatchableEntry :=
		(arch == "arm64" && (enabled(config, "CONFIG_DYNAMIC_FTRACE_WITH_CALL_OPS") || enabled(config, "CONFIG_DYNAMIC_FTRACE_WITH_ARGS"))) ||
			(arch == "riscv" && enabled(config, "CONFIG_DYNAMIC_FTRACE")) ||
			(arch == "powerpc" && enabled(config, "CONFIG_ARCH_USING_PATCHABLE_FUNCTION_ENTRY"))
	if !usesPatchableEntry {
		flags = append(flags, "-pg")
	}
	if enabled(config, "CONFIG_FTRACE_MCOUNT_USE_CC") {
		flags = append(flags, "-mrecord-mcount")
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

func debugInfoFlags(config map[string]string) []string {
	if !enabled(config, "CONFIG_DEBUG_INFO") {
		return nil
	}
	flags := []string{"-g"}
	switch {
	case enabled(config, "CONFIG_DEBUG_INFO_DWARF4"):
		flags = append(flags, "-gdwarf-4")
	case enabled(config, "CONFIG_DEBUG_INFO_DWARF5"):
		flags = append(flags, "-gdwarf-5")
	}
	return flags
}

func x86CFlags(config map[string]string, version string) []string {
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
		// Linux 6.15 converted the fixed %gs:40 canary into a normal
		// per-CPU symbol and began overriding the compiler's guard location.
		if kernelVersionAtLeast(version, 6, 15) {
			flags = append(flags, x86StackProtectorGuardFlags(config, "gs")...)
		}
	} else {
		flags = append(flags, "-m32", "-msoft-float", "-mregparm=3", "-freg-struct-return", "-fno-pic")
		flags = append(flags, x86StackProtectorGuardFlags(config, "fs")...)
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

func x86StackProtectorGuardFlags(config map[string]string, register string) []string {
	if !enabled(config, "CONFIG_STACKPROTECTOR") {
		return nil
	}
	if !enabled(config, "CONFIG_SMP") {
		return []string{"-mstack-protector-guard=global"}
	}
	return []string{
		"-mstack-protector-guard-reg=" + register,
		"-mstack-protector-guard-symbol=__ref_stack_chk_guard",
	}
}

func kernelVersionAtLeast(version string, wantMajor, wantMinor int) bool {
	version = strings.TrimPrefix(version, "v")
	majorText, rest, ok := strings.Cut(version, ".")
	if !ok {
		return false
	}
	minorEnd := 0
	for minorEnd < len(rest) && rest[minorEnd] >= '0' && rest[minorEnd] <= '9' {
		minorEnd++
	}
	if minorEnd == 0 {
		return false
	}
	major, majorErr := strconv.Atoi(majorText)
	minor, minorErr := strconv.Atoi(rest[:minorEnd])
	if majorErr != nil || minorErr != nil {
		return false
	}
	return major > wantMajor || (major == wantMajor && minor >= wantMinor)
}

func linuxAFlags(config map[string]string, arch, version string) []string {
	switch arch {
	case "arm":
		return append(armAFlags(config), debugInfoFlags(config)...)
	case "arm64":
		return append(arm64AFlags(config), debugInfoFlags(config)...)
	case "powerpc":
		return append(powerPCAFlags(config), debugInfoFlags(config)...)
	case "riscv":
		return append(riscvAFlags(config), debugInfoFlags(config)...)
	case "x86":
		return append(append(x86CFlags(config, version), ftraceUsingFlags(config)...), debugInfoFlags(config)...)
	default:
		return nil
	}
}

func armArchitecture(config map[string]string) (string, string) {
	march := ""
	version := ""
	// Keep the assignments in arch/arm/Makefile order. Multi-platform
	// configurations can select more than one CPU generation, and Kbuild's
	// later assignment intentionally chooses their common denominator.
	for _, spec := range []struct {
		key     string
		march   string
		version string
	}{
		{"CONFIG_CPU_32v7M", "armv7-m", "7"},
		{"CONFIG_CPU_32v7", "armv7-a", "7"},
		{"CONFIG_CPU_32v6", "armv6", "6"},
	} {
		if enabled(config, spec.key) {
			march = spec.march
			version = spec.version
		}
	}
	if enabled(config, "CONFIG_CPU_32v6") && enabled(config, "CONFIG_CPU_32v6K") {
		march = "armv6k"
		version = "6"
	}
	for _, spec := range []struct {
		key     string
		march   string
		version string
	}{
		{"CONFIG_CPU_32v5", "armv5te", "5"},
		{"CONFIG_CPU_32v4T", "armv4t", "4"},
		{"CONFIG_CPU_32v4", "armv4", "4"},
		{"CONFIG_CPU_32v3", "armv3m", "3"},
	} {
		if enabled(config, spec.key) {
			march = spec.march
			version = spec.version
		}
	}
	return march, version
}

func armTune(config map[string]string) string {
	for _, spec := range []struct {
		key  string
		tune string
	}{
		{"CONFIG_CPU_V6K", "arm1136j-s"},
		{"CONFIG_CPU_V6", "arm1136j-s"},
		{"CONFIG_CPU_FEROCEON", "xscale"},
		{"CONFIG_CPU_XSC3", "xscale"},
		{"CONFIG_CPU_XSCALE", "xscale"},
		{"CONFIG_CPU_SA1100", "strongarm1100"},
		{"CONFIG_CPU_SA110", "strongarm110"},
		{"CONFIG_CPU_FA526", "arm9tdmi"},
		{"CONFIG_CPU_ARM926T", "arm9tdmi"},
		{"CONFIG_CPU_ARM925T", "arm9tdmi"},
		{"CONFIG_CPU_ARM922T", "arm9tdmi"},
		{"CONFIG_CPU_ARM920T", "arm9tdmi"},
		{"CONFIG_CPU_ARM946E", "arm9e"},
		{"CONFIG_CPU_ARM940T", "arm9tdmi"},
		{"CONFIG_CPU_ARM9TDMI", "arm9tdmi"},
		{"CONFIG_CPU_ARM740T", "arm7tdmi"},
		{"CONFIG_CPU_ARM720T", "arm7tdmi"},
		{"CONFIG_CPU_ARM7TDMI", "arm7tdmi"},
	} {
		if enabled(config, spec.key) {
			return spec.tune
		}
	}
	return ""
}

func armABIFlags(config map[string]string) []string {
	flags := []string{}
	if enabled(config, "CONFIG_AEABI") {
		flags = append(flags, "-mabi=aapcs-linux", "-mfpu=vfp")
	} else {
		flags = append(flags, "-mabi=apcs-gnu", "-mno-thumb-interwork")
	}
	if enabled(config, "CONFIG_ARM_UNWIND") {
		flags = append(flags, "-funwind-tables")
	}
	if enabled(config, "CONFIG_CC_IS_CLANG") {
		flags = append(flags, "-meabi", "gnu")
	}
	return flags
}

func armISAFlags(config map[string]string, assembly bool) []string {
	flags := []string{"-Wa,-W"}
	if enabled(config, "CONFIG_THUMB2_KERNEL") {
		flags = append(flags, "-Wa,-mimplicit-it=always")
		if assembly {
			flags = append(flags, "-Wa,-mthumb")
		} else {
			flags = append(flags, "-mthumb")
		}
	} else {
		flags = append(flags, "-marm")
	}
	return flags
}

func armCFlags(config map[string]string) []string {
	flags := []string{
		"-fno-dwarf2-cfi-asm",
	}
	if enabled(config, "CONFIG_CPU_BIG_ENDIAN") {
		flags = append(flags, "-mbig-endian")
	} else {
		flags = append(flags, "-mlittle-endian")
	}
	if !enabled(config, "CONFIG_MMU") {
		flags = append(flags, "-mno-unaligned-access")
	}
	if enabled(config, "CONFIG_FRAME_POINTER") && !enabled(config, "CONFIG_CC_IS_CLANG") {
		flags = append(flags, "-mapcs", "-mno-sched-prolog")
	}
	if enabled(config, "CONFIG_CURRENT_POINTER_IN_TPIDRURO") {
		flags = append(flags, "-mtp=cp15")
	}
	flags = append(flags, armABIFlags(config)...)
	flags = append(flags, armISAFlags(config, false)...)
	march, version := armArchitecture(config)
	if march != "" {
		flags = append(flags, "-march="+march)
	}
	if version != "" {
		flags = append(flags, "-D__LINUX_ARM_ARCH__="+version)
	}
	if tune := armTune(config); tune != "" {
		flags = append(flags, "-mtune="+tune)
	}
	return append(flags, "-msoft-float", "-Uarm")
}

func armAFlags(config map[string]string) []string {
	flags := []string{"-D__ASSEMBLY__", "-fno-PIE"}
	if enabled(config, "CONFIG_CPU_BIG_ENDIAN") {
		flags = append(flags, "-mbig-endian")
	} else {
		flags = append(flags, "-mlittle-endian")
	}
	flags = append(flags, armABIFlags(config)...)
	flags = append(flags, armISAFlags(config, true)...)
	if march, version := armArchitecture(config); march != "" {
		flags = append(flags, "-Wa,-march="+march)
		if version != "" {
			flags = append(flags, "-D__LINUX_ARM_ARCH__="+version)
		}
	}
	if tune := armTune(config); tune != "" {
		flags = append(flags, "-mtune="+tune)
	}
	return append(flags, "-msoft-float")
}

func riscvMarch(config map[string]string, assembly bool) string {
	march := "rv64ima"
	if assembly && enabled(config, "CONFIG_FPU") {
		march += "fd"
	}
	if enabled(config, "CONFIG_RISCV_ISA_C") {
		march += "c"
	}
	if assembly && enabled(config, "CONFIG_RISCV_ISA_V") {
		march += "v"
	}
	if !enabled(config, "CONFIG_TOOLCHAIN_NEEDS_OLD_ISA_SPEC") && enabled(config, "CONFIG_TOOLCHAIN_NEEDS_EXPLICIT_ZICSR_ZIFENCEI") {
		march += "_zicsr_zifencei"
	}
	if enabled(config, "CONFIG_TOOLCHAIN_HAS_ZACAS") {
		march += "_zacas"
	}
	if enabled(config, "CONFIG_TOOLCHAIN_HAS_ZABHA") {
		march += "_zabha"
	}
	return march
}

func riscvCFlags(config map[string]string, version string) []string {
	flags := []string{
		"-mabi=lp64",
		"-march=" + riscvMarch(config, false),
		"-mno-save-restore",
		"-fno-asynchronous-unwind-tables",
		"-fno-unwind-tables",
	}
	if enabled(config, "CONFIG_TOOLCHAIN_NEEDS_OLD_ISA_SPEC") {
		flags = append(flags, "-Wa,-misa-spec=2.2")
	}
	if enabled(config, "CONFIG_RELOCATABLE") {
		flags = append(flags, "-fPIE")
	}
	if enabled(config, "CONFIG_CMODEL_MEDLOW") {
		flags = append(flags, "-mcmodel=medlow")
	}
	if enabled(config, "CONFIG_CMODEL_MEDANY") {
		flags = append(flags, "-mcmodel=medany")
	}
	if !enabled(config, "CONFIG_HAVE_EFFICIENT_UNALIGNED_ACCESS") {
		flags = append(flags, "-mstrict-align")
	}
	if enabled(config, "CONFIG_DYNAMIC_FTRACE") {
		flags = append(flags, "-DCC_USING_PATCHABLE_FUNCTION_ENTRY")
		if kernelVersionAtLeast(version, 6, 16) {
			if enabled(config, "CONFIG_RISCV_ISA_C") {
				flags = append(flags, "-fpatchable-function-entry=8,4")
			} else {
				flags = append(flags, "-fpatchable-function-entry=4,2")
			}
		} else if enabled(config, "CONFIG_RISCV_ISA_C") {
			flags = append(flags, "-fpatchable-function-entry=4")
		} else {
			flags = append(flags, "-fpatchable-function-entry=2")
		}
	}
	return flags
}

func riscvAFlags(config map[string]string) []string {
	flags := []string{
		"-D__ASSEMBLY__",
		"-fno-PIE",
		"-mabi=lp64",
		"-march=" + riscvMarch(config, true),
	}
	if enabled(config, "CONFIG_TOOLCHAIN_NEEDS_OLD_ISA_SPEC") {
		flags = append(flags, "-Wa,-misa-spec=2.2")
	}
	return flags
}

func powerPCCommonFlags(config map[string]string) []string {
	flags := []string{"-m64"}
	if enabled(config, "CONFIG_CPU_BIG_ENDIAN") {
		flags = append(flags, "-mbig-endian")
	} else {
		flags = append(flags, "-mlittle-endian")
	}
	if enabled(config, "CONFIG_PPC64_ELF_ABI_V2") {
		flags = append(flags, "-mabi=elfv2")
	} else if enabled(config, "CONFIG_PPC64_ELF_ABI_V1") {
		flags = append(flags, "-mabi=elfv1")
	}
	if enabled(config, "CONFIG_TARGET_CPU_BOOL") {
		if cpu := unquote(config["CONFIG_TARGET_CPU"]); cpu != "" {
			flags = append(flags, "-mcpu="+cpu)
		}
	}
	return flags
}

func powerPCCFlags(config map[string]string) []string {
	flags := powerPCCommonFlags(config)
	flags = append(flags,
		"-mcmodel=medium",
		"-mlong-double-128",
		"-msoft-float",
		"-mno-altivec",
		"-mno-vsx",
		"-mno-mma",
		"-mno-spe",
		"-fno-asynchronous-unwind-tables",
	)
	if tune := unquote(config["CONFIG_TUNE_CPU"]); tune != "" {
		flags = append(flags, tune)
	}
	if enabled(config, "CONFIG_PPC_KERNEL_PREFIXED") {
		flags = append(flags, "-mprefixed")
	} else {
		flags = append(flags, "-mno-prefixed")
	}
	if enabled(config, "CONFIG_PPC_KERNEL_PCREL") {
		flags = append(flags, "-mpcrel")
	} else {
		flags = append(flags, "-mno-pcrel")
	}
	if enabled(config, "CONFIG_FUNCTION_TRACER") {
		if enabled(config, "CONFIG_ARCH_USING_PATCHABLE_FUNCTION_ENTRY") {
			flags = append(flags, "-DCC_USING_PATCHABLE_FUNCTION_ENTRY")
			if enabled(config, "CONFIG_PPC_FTRACE_OUT_OF_LINE") {
				flags = append(flags, "-fpatchable-function-entry=1")
			} else if enabled(config, "CONFIG_DYNAMIC_FTRACE_WITH_CALL_OPS") {
				flags = append(flags, "-fpatchable-function-entry=3,1")
			} else {
				flags = append(flags, "-fpatchable-function-entry=2")
			}
		} else if enabled(config, "CONFIG_MPROFILE_KERNEL") {
			flags = append(flags, "-mprofile-kernel")
		}
	}
	return flags
}

func powerPCAFlags(config map[string]string) []string {
	return append([]string{"-D__ASSEMBLY__", "-fno-PIE"}, powerPCCommonFlags(config)...)
}

func arm64CFlags(config map[string]string) []string {
	flags := []string{
		"-mgeneral-regs-only",
		"-DCONFIG_CC_HAS_K_CONSTRAINT=1",
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
	flags = append(flags, "-DKASAN_SHADOW_SCALE_SHIFT="+kasanShift)
	return flags
}

func arm64AFlags(config map[string]string) []string {
	flags := []string{
		"-D__ASSEMBLY__",
		"-fno-PIE",
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
	if enabled(config, "CONFIG_SHADOW_CALL_STACK") {
		flags = append(flags, "-ffixed-x18")
	}
	kasanShift := ""
	if enabled(config, "CONFIG_KASAN_SW_TAGS") {
		kasanShift = "4"
	} else if enabled(config, "CONFIG_KASAN_GENERIC") {
		kasanShift = "3"
	}
	flags = append(flags, "-DKASAN_SHADOW_SCALE_SHIFT="+kasanShift)
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
