package main

import (
	"bufio"
	"bytes"
	"debug/elf"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func main() {
	arch := flag.String("arch", "", "Linux ARCH value")
	asmOffsets := flag.String("asm_offsets", "", "include/generated/asm-offsets.h input")
	configPath := flag.String("config", "", "Resolved Linux .config input")
	cflagsOut := flag.String("cflags_out", "", "Generated architecture C flags response file")
	armVmlinux := flag.String("arm_vmlinux", "", "ARM vmlinux ELF used to derive compressed-image link symbols")
	armLinkFlagsOut := flag.String("arm_link_flags_out", "", "Generated ARM compressed-image linker response file")
	vdsoELF := flag.String("riscv_vdso_elf", "", "RISC-V vDSO ELF used to derive symbol offsets")
	vdsoOffsetsOut := flag.String("riscv_vdso_offsets_out", "", "Generated RISC-V vDSO offsets header")
	vdsoCompat := flag.Bool("riscv_vdso_compat", false, "Prefix RISC-V vDSO offsets for the compat image")
	machTypes := flag.String("mach_types", "", "arch/arm/tools/mach-types input")
	machTypesOut := flag.String("mach_types_out", "", "Generated asm/mach-types.h output")
	syscallTable := flag.String("syscall_table", "", "arch/arm/tools/syscall.tbl input")
	syscallNROut := flag.String("syscall_nr_out", "", "Generated asm/unistd-nr.h output")
	flag.Parse()

	cflagsMode := *arch != "" || *asmOffsets != "" || *cflagsOut != ""
	armMode := *machTypes != "" || *machTypesOut != "" || *syscallTable != "" || *syscallNROut != ""
	armLinkMode := *armVmlinux != "" || *armLinkFlagsOut != ""
	vdsoOffsetsMode := *vdsoELF != "" || *vdsoOffsetsOut != "" || *vdsoCompat
	if cflagsMode != (*arch != "" && *asmOffsets != "" && *configPath != "" && *cflagsOut != "") ||
		armMode != (*machTypes != "" && *machTypesOut != "" && *syscallTable != "" && *syscallNROut != "") ||
		armLinkMode != (*armVmlinux != "" && *armLinkFlagsOut != "" && *configPath != "") ||
		vdsoOffsetsMode != (*vdsoELF != "" && *vdsoOffsetsOut != "") ||
		(!cflagsMode && !armMode && !armLinkMode && !vdsoOffsetsMode) {
		flag.PrintDefaults()
		os.Exit(2)
	}

	if armMode {
		machData, err := os.ReadFile(*machTypes)
		if err != nil {
			fatal("read mach-types", err)
		}
		generated, err := generateMachTypes(machData)
		if err != nil {
			fatal("generate mach-types", err)
		}
		if err := writeFile(*machTypesOut, generated); err != nil {
			fatal("write mach-types", err)
		}
		syscallData, err := os.ReadFile(*syscallTable)
		if err != nil {
			fatal("read syscall table", err)
		}
		generated, err = generateARMSyscallNR(syscallData)
		if err != nil {
			fatal("generate syscall count", err)
		}
		if err := writeFile(*syscallNROut, generated); err != nil {
			fatal("write syscall count", err)
		}
	}

	if cflagsMode {
		config := readConfig(*configPath)
		offsets, err := os.ReadFile(*asmOffsets)
		if err != nil {
			fatal("read asm offsets", err)
		}
		generated, err := generateArchitectureCFlags(*arch, config, offsets)
		if err != nil {
			fatal("generate architecture C flags", err)
		}
		if err := writeFile(*cflagsOut, generated); err != nil {
			fatal("write architecture C flags", err)
		}
	}

	if armLinkMode {
		config := readConfig(*configPath)
		generated, err := generateARMCompressedLinkFlags(*armVmlinux, config)
		if err != nil {
			fatal("generate ARM compressed-image link flags", err)
		}
		if err := writeFile(*armLinkFlagsOut, generated); err != nil {
			fatal("write ARM compressed-image link flags", err)
		}
	}

	if vdsoOffsetsMode {
		generated, err := generateRISCVVDSOOffsets(*vdsoELF, *vdsoCompat)
		if err != nil {
			fatal("generate RISC-V vDSO offsets", err)
		}
		if err := writeFile(*vdsoOffsetsOut, generated); err != nil {
			fatal("write RISC-V vDSO offsets", err)
		}
	}
}

func readConfig(path string) map[string]string {
	configFile, err := os.Open(path)
	if err != nil {
		fatal("open config", err)
	}
	config, err := kconfig.ParseConfig(configFile)
	closeErr := configFile.Close()
	if err != nil {
		fatal("parse config", err)
	}
	if closeErr != nil {
		fatal("close config", closeErr)
	}
	return config
}

func fatal(action string, err error) {
	fmt.Fprintf(os.Stderr, "archheaders: %s: %v\n", action, err)
	os.Exit(1)
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

type machineType struct {
	name   string
	config string
	macro  string
	number string
}

func generateMachTypes(data []byte) ([]byte, error) {
	var machines []machineType
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 && len(fields) != 4 {
			return nil, fmt.Errorf("line %d: expected three or four fields", lineNumber)
		}
		machine := machineType{
			name:   "machine_is_" + fields[0],
			config: "CONFIG_" + fields[1],
			macro:  "MACH_TYPE_" + fields[2],
		}
		if len(fields) == 4 {
			if _, err := strconv.ParseUint(fields[3], 0, 32); err != nil {
				return nil, fmt.Errorf("line %d: invalid machine number %q: %w", lineNumber, fields[3], err)
			}
			machine.number = fields[3]
		}
		machines = append(machines, machine)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.WriteString("/* Generated from arch/arm/tools/mach-types. Do not edit. */\n\n")
	out.WriteString("#ifndef __ASM_ARM_MACH_TYPE_H\n#define __ASM_ARM_MACH_TYPE_H\n\n")
	out.WriteString("#ifndef __ASSEMBLY__\nextern unsigned int __machine_arch_type;\n#endif\n\n")
	for _, machine := range machines {
		if machine.number != "" {
			fmt.Fprintf(&out, "#define %-30s %s\n", machine.macro, machine.number)
		}
	}
	out.WriteByte('\n')
	for _, machine := range machines {
		if machine.number == "" {
			continue
		}
		fmt.Fprintf(&out, "#ifdef %s\n", machine.config)
		out.WriteString("# ifdef machine_arch_type\n#  undef machine_arch_type\n#  define machine_arch_type\t__machine_arch_type\n# else\n")
		fmt.Fprintf(&out, "#  define machine_arch_type\t%s\n", machine.macro)
		out.WriteString("# endif\n")
		fmt.Fprintf(&out, "# define %s()\t(machine_arch_type == %s)\n", machine.name, machine.macro)
		out.WriteString("#else\n")
		fmt.Fprintf(&out, "# define %s()\t(0)\n", machine.name)
		out.WriteString("#endif\n\n")
	}
	for _, machine := range machines {
		if machine.number == "" {
			fmt.Fprintf(&out, "#define %s()\t(0)\n", machine.name)
		}
	}
	out.WriteString("\n#ifndef machine_arch_type\n#define machine_arch_type\t__machine_arch_type\n#endif\n\n#endif\n")
	return out.Bytes(), nil
}

func generateARMSyscallNR(data []byte) ([]byte, error) {
	max := int64(-1)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		nr, err := strconv.ParseInt(fields[0], 0, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid syscall number %q: %w", fields[0], err)
		}
		if nr > max {
			max = nr
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if max < 0 {
		return nil, fmt.Errorf("syscall table contains no syscall numbers")
	}
	nr := max + 1
	align := int64(1)
	for nr/(256*align) > 0 {
		align *= 4
	}
	nr = (nr + align - 1) & ^(align - 1)
	return []byte(fmt.Sprintf("#ifndef _ASM_ARM_UNISTD_NR_H\n#define _ASM_ARM_UNISTD_NR_H 1\n\n/* aligned to %d */\n#define __NR_syscalls %d\n\n#endif /* _ASM_ARM_UNISTD_NR_H */\n", align, nr)), nil
}

func generateArchitectureCFlags(arch string, config map[string]string, offsets []byte) ([]byte, error) {
	type stackProtectorSpec struct {
		enabled  bool
		offset   string
		prefixes []string
	}
	var spec stackProtectorSpec
	switch arch {
	case "arm":
		spec = stackProtectorSpec{
			enabled: config["CONFIG_STACKPROTECTOR_PER_TASK"] == "y" && config["CONFIG_CC_HAVE_STACKPROTECTOR_TLS"] == "y",
			offset:  "TSK_STACK_CANARY",
			prefixes: []string{
				"-mstack-protector-guard=tls",
			},
		}
	case "riscv":
		spec = stackProtectorSpec{
			enabled: config["CONFIG_STACKPROTECTOR_PER_TASK"] == "y",
			offset:  "TSK_STACK_CANARY",
			prefixes: []string{
				"-mstack-protector-guard=tls",
				"-mstack-protector-guard-reg=tp",
			},
		}
	case "powerpc":
		spec = stackProtectorSpec{
			enabled: config["CONFIG_STACKPROTECTOR"] == "y",
			offset:  "PACA_CANARY",
			prefixes: []string{
				"-mstack-protector-guard=tls",
				"-mstack-protector-guard-reg=r13",
			},
		}
	default:
		return nil, fmt.Errorf("unsupported Linux ARCH %q", arch)
	}
	if !spec.enabled {
		return nil, nil
	}
	offset, err := findOffset(offsets, spec.offset)
	if err != nil {
		return nil, err
	}
	flags := append([]string(nil), spec.prefixes...)
	flags = append(flags, fmt.Sprintf("-mstack-protector-guard-offset=%d", offset))
	return []byte(strings.Join(flags, "\n") + "\n"), nil
}

func generateARMCompressedLinkFlags(vmlinuxPath string, config map[string]string) ([]byte, error) {
	file, err := elf.Open(vmlinuxPath)
	if err != nil {
		return nil, fmt.Errorf("open vmlinux ELF: %w", err)
	}
	defer file.Close()
	symbols, err := file.Symbols()
	if err != nil {
		return nil, fmt.Errorf("read vmlinux symbols: %w", err)
	}
	bssSize, err := armKernelBSSSize(symbols)
	if err != nil {
		return nil, err
	}
	flags := []string{
		"--defsym",
		fmt.Sprintf("_kernel_bss_size=%d", bssSize),
	}
	if config["CONFIG_AUTO_ZRELADDR"] != "y" {
		physOffset, err := configInteger(config, "CONFIG_PHYS_OFFSET")
		if err != nil {
			return nil, err
		}
		flags = append(flags,
			"--defsym",
			fmt.Sprintf("zreladdr=0x%x", physOffset+armTextOffset(config)),
		)
	}
	return []byte(strings.Join(flags, "\n") + "\n"), nil
}

func generateRISCVVDSOOffsets(vdsoPath string, compat bool) ([]byte, error) {
	file, err := elf.Open(vdsoPath)
	if err != nil {
		return nil, fmt.Errorf("open vDSO ELF: %w", err)
	}
	defer file.Close()
	symbols, err := file.Symbols()
	if err != nil {
		return nil, fmt.Errorf("read vDSO symbols: %w", err)
	}
	return riscvVDSOOffsets(symbols, compat)
}

func riscvVDSOOffsets(symbols []elf.Symbol, compat bool) ([]byte, error) {
	lines := []string{}
	for _, symbol := range symbols {
		if symbol.Section == elf.SHN_UNDEF || !validVDSOSymbol(symbol.Name) {
			continue
		}
		name := symbol.Name
		if compat {
			name = "compat" + name
		}
		lines = append(lines, fmt.Sprintf("#define %s_offset\t0x%x", name, symbol.Value))
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("vDSO ELF contains no __vdso_* symbols")
	}
	sort.Strings(lines)
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func validVDSOSymbol(name string) bool {
	const prefix = "__vdso_"
	if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
		return false
	}
	for _, char := range name[len(prefix):] {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func armKernelBSSSize(symbols []elf.Symbol) (uint64, error) {
	values := map[string]uint64{}
	for _, symbol := range symbols {
		if symbol.Section != elf.SHN_UNDEF && (symbol.Name == "__bss_start" || symbol.Name == "__bss_stop") {
			values[symbol.Name] = symbol.Value
		}
	}
	start, hasStart := values["__bss_start"]
	stop, hasStop := values["__bss_stop"]
	if !hasStart || !hasStop {
		return 0, fmt.Errorf("vmlinux is missing __bss_start or __bss_stop")
	}
	if stop < start {
		return 0, fmt.Errorf("vmlinux __bss_stop (0x%x) precedes __bss_start (0x%x)", stop, start)
	}
	return stop - start, nil
}

func armTextOffset(config map[string]string) uint64 {
	switch {
	case config["CONFIG_ARCH_AXXIA"] == "y":
		return 0x00308000
	case config["CONFIG_ARCH_QCOM_RESERVE_SMEM"] == "y", config["CONFIG_ARCH_MESON"] == "y":
		return 0x00208000
	case config["CONFIG_ARCH_SA1100"] == "y" && config["CONFIG_SA1111"] == "y":
		return 0x00208000
	case config["CONFIG_ARCH_REALTEK"] == "y":
		return 0x00108000
	default:
		return 0x00008000
	}
}

func configInteger(config map[string]string, key string) (uint64, error) {
	value := strings.Trim(config[key], `"`)
	if value == "" {
		return 0, fmt.Errorf("resolved config is missing %s", key)
	}
	parsed, err := strconv.ParseUint(value, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q: %w", key, config[key], err)
	}
	return parsed, nil
}

func findOffset(data []byte, name string) (int64, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || fields[0] != "#define" || fields[1] != name {
			continue
		}
		value := strings.TrimFunc(fields[2], func(r rune) bool { return !unicode.IsDigit(r) && r != '-' })
		offset, err := strconv.ParseInt(value, 0, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid %s offset %q: %w", name, fields[2], err)
		}
		return offset, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s not found in asm offsets", name)
}
