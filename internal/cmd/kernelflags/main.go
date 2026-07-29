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
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

type configPayloadManifest struct {
	Arch     string            `json:"arch"`
	Payloads map[string]string `json:"payloads"`
	Version  string            `json:"version"`
}

type repeatedStringFlag []string

func (values *repeatedStringFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *repeatedStringFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	configPath := flag.String("config", "", "Resolved Linux .config file")
	arch := flag.String("arch", "x86", "Linux ARCH value")
	version := flag.String("version", "", "Linux kernel version")
	outPath := flag.String("out", "", "Output Clang response file")
	asmOutPath := flag.String("asm_out", "", "Output assembler response file")
	batchManifestPath := flag.String("batch_manifest", "", "JSON manifest of content-addressed config payloads")
	batchOutDir := flag.String("batch_out_dir", "", "Output root for content-addressed config payloads")
	var batchPayloads repeatedStringFlag
	flag.Var(&batchPayloads, "batch_payload", "Content-addressed config payload as ID=path; repeat for batching")
	flag.Parse()

	if *batchManifestPath != "" || *batchOutDir != "" || len(batchPayloads) != 0 {
		if *batchOutDir == "" || *configPath != "" || *outPath != "" || *asmOutPath != "" ||
			(*batchManifestPath == "") == (len(batchPayloads) == 0) {
			flag.PrintDefaults()
			os.Exit(2)
		}
		var err error
		if *batchManifestPath != "" {
			err = materializeConfigPayloads(*batchManifestPath, *batchOutDir)
		} else {
			err = materializeConfigPayloadFiles(
				batchPayloads,
				*batchOutDir,
				*arch,
				*version,
			)
		}
		if err != nil {
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

func materializeConfigPayloadFiles(
	encoded []string,
	outDir string,
	arch string,
	version string,
) error {
	if arch != "" && arch != "arm64" && arch != "x86" {
		return fmt.Errorf("unsupported Linux ARCH %q", arch)
	}
	payloads := make(map[string]string, len(encoded))
	for _, value := range encoded {
		id, path, ok := strings.Cut(value, "=")
		if !ok || path == "" {
			return fmt.Errorf("invalid batch payload %q; want ID=path", value)
		}
		if !isContentID(id) {
			return fmt.Errorf("invalid payload ID %q", id)
		}
		if _, ok := payloads[id]; ok {
			return fmt.Errorf("repeated payload ID %s", id)
		}
		payloads[id] = path
	}
	ids := make([]string, 0, len(payloads))
	for id := range payloads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		content, err := os.ReadFile(payloads[id])
		if err != nil {
			return fmt.Errorf("read payload %s: %w", id, err)
		}
		config, err := parseConfigPayload(string(content))
		if err != nil {
			return fmt.Errorf("parse payload %s: %w", id, err)
		}
		if err := materializeConfigPayload(outDir, id, config, arch, version); err != nil {
			return fmt.Errorf("materialize payload %s: %w", id, err)
		}
	}
	return nil
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
	if manifest.Arch != "" && manifest.Arch != "arm64" && manifest.Arch != "x86" {
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
		filepath.Join(root, "include", "generated", "bazel_kbuild_aflags.rsp"): configFlagsResponse(config, arch, true),
		filepath.Join(root, "include", "generated", "bazel_kbuild_cflags.rsp"): configFlagsResponse(config, arch, false),
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

func configFlagsResponse(config map[string]string, arch string, assembly bool) string {
	if arch == "" {
		return ""
	}
	if assembly {
		return responseFile(linuxAFlags(config, arch))
	}
	return responseFile(linuxCFlags(config, arch))
}

func unquote(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
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
	if arch == "x86" {
		flags = append(flags, x86CFlags(config)...)
	}
	if arch == "arm64" {
		flags = append(flags, arm64CFlags(config)...)
	}
	return flags
}

func ftraceFlags(config map[string]string, arch string) []string {
	if !enabled(config, "CONFIG_FUNCTION_TRACER") {
		return nil
	}
	flags := []string{}
	if !(arch == "arm64" && (enabled(config, "CONFIG_DYNAMIC_FTRACE_WITH_CALL_OPS") || enabled(config, "CONFIG_DYNAMIC_FTRACE_WITH_ARGS"))) {
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
		} else if enabled(config, "CONFIG_STACKPROTECTOR") {
			flags = append(flags, "-mstack-protector-guard=global")
		}
	} else {
		flags = append(flags, "-m32", "-msoft-float", "-mregparm=3", "-freg-struct-return", "-fno-pic")
		if enabled(config, "CONFIG_STACKPROTECTOR") && enabled(config, "CONFIG_SMP") {
			flags = append(flags,
				"-mstack-protector-guard-reg=fs",
				"-mstack-protector-guard-symbol=__ref_stack_chk_guard",
			)
		} else if enabled(config, "CONFIG_STACKPROTECTOR") {
			flags = append(flags, "-mstack-protector-guard=global")
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
		return append(arm64AFlags(config), debugInfoFlags(config)...)
	case "x86":
		return append(append(x86CFlags(config), ftraceUsingFlags(config)...), debugInfoFlags(config)...)
	default:
		return nil
	}
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
	asmArch := "armv8.4-a"
	if enabled(config, "CONFIG_AS_HAS_ARMV8_5") {
		asmArch = "armv8.5-a"
	}
	flags = append(flags, "-Wa,-march="+asmArch, "-DARM64_ASM_ARCH=\\\""+asmArch+"\\\"")
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
