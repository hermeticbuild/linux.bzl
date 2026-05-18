// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"linux.bzl/internal/kconfig"
)

func main() {
	cpucapsIn := flag.String("cpucaps", "", "arch/arm64/tools/cpucaps input")
	cpucapsOut := flag.String("cpucaps_out", "", "Generated cpucap-defs.h output")
	sysregIn := flag.String("sysreg", "", "arch/arm64/tools/sysreg input")
	sysregOut := flag.String("sysreg_out", "", "Generated sysreg-defs.h output")
	asmOffsetsIn := flag.String("asm_offsets", "", "include/generated/asm-offsets.h input")
	stackProtectorConfig := flag.String("stackprotector_config", "", "Resolved Linux .config input for stack protector flags")
	stackProtectorOut := flag.String("stackprotector_out", "", "Generated arm64 stack protector response file output")
	vdsoNMIn := flag.String("vdso_nm", "", "llvm-nm output for arm64 vDSO")
	vdsoOffsetsOut := flag.String("vdso_offsets_out", "", "Generated include/generated/vdso-offsets.h output")
	flag.Parse()

	if (*cpucapsIn == "") != (*cpucapsOut == "") || (*sysregIn == "") != (*sysregOut == "") || (*asmOffsetsIn == "") != (*stackProtectorConfig == "") || (*asmOffsetsIn == "") != (*stackProtectorOut == "") || (*vdsoNMIn == "") != (*vdsoOffsetsOut == "") {
		flag.PrintDefaults()
		os.Exit(2)
	}
	if *cpucapsIn == "" && *sysregIn == "" && *asmOffsetsIn == "" && *vdsoNMIn == "" {
		flag.PrintDefaults()
		os.Exit(2)
	}

	if *cpucapsIn != "" {
		data, err := os.ReadFile(*cpucapsIn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read cpucaps: %v\n", err)
			os.Exit(1)
		}
		out, err := generateCPUCapDefs(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "generate cpucaps: %v\n", err)
			os.Exit(1)
		}
		if err := writeFile(*cpucapsOut, out); err != nil {
			fmt.Fprintf(os.Stderr, "write cpucaps: %v\n", err)
			os.Exit(1)
		}
	}
	if *sysregIn != "" {
		data, err := os.ReadFile(*sysregIn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read sysreg: %v\n", err)
			os.Exit(1)
		}
		out, err := generateSysregDefs(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "generate sysreg: %v\n", err)
			os.Exit(1)
		}
		if err := writeFile(*sysregOut, out); err != nil {
			fmt.Fprintf(os.Stderr, "write sysreg: %v\n", err)
			os.Exit(1)
		}
	}
	if *asmOffsetsIn != "" {
		configFile, err := os.Open(*stackProtectorConfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open stack protector config: %v\n", err)
			os.Exit(1)
		}
		config, err := kconfig.ParseConfig(configFile)
		closeErr := configFile.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse stack protector config: %v\n", err)
			os.Exit(1)
		}
		if closeErr != nil {
			fmt.Fprintf(os.Stderr, "close stack protector config: %v\n", closeErr)
			os.Exit(1)
		}
		data, err := os.ReadFile(*asmOffsetsIn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read asm offsets: %v\n", err)
			os.Exit(1)
		}
		out, err := generateStackProtectorFlags(config, data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "generate stack protector flags: %v\n", err)
			os.Exit(1)
		}
		if err := writeFile(*stackProtectorOut, out); err != nil {
			fmt.Fprintf(os.Stderr, "write stack protector flags: %v\n", err)
			os.Exit(1)
		}
	}
	if *vdsoNMIn != "" {
		data, err := os.ReadFile(*vdsoNMIn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read vdso nm: %v\n", err)
			os.Exit(1)
		}
		out, err := generateVDSOOffsets(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "generate vdso offsets: %v\n", err)
			os.Exit(1)
		}
		if err := writeFile(*vdsoOffsetsOut, out); err != nil {
			fmt.Fprintf(os.Stderr, "write vdso offsets: %v\n", err)
			os.Exit(1)
		}
	}
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func generateCPUCapDefs(data []byte) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString("#ifndef __ASM_CPUCAP_DEFS_H\n")
	out.WriteString("#define __ASM_CPUCAP_DEFS_H\n\n")
	out.WriteString("/* Generated file - do not edit */\n\n")

	scanner := bufio.NewScanner(bytes.NewReader(data))
	capNum := 0
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !isCPUCapName(line) {
			return nil, fmt.Errorf("line %d: unhandled statement %q", lineNum, line)
		}
		fmt.Fprintf(&out, "#define ARM64_%-40s\t%d\n", line, capNum)
		capNum++
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	fmt.Fprintf(&out, "#define ARM64_NCAPS\t\t\t\t\t%d\n\n", capNum)
	out.WriteString("#endif /* __ASM_CPUCAP_DEFS_H */\n")
	return out.Bytes(), nil
}

func generateStackProtectorFlags(config map[string]string, data []byte) ([]byte, error) {
	if !enabled(config, "CONFIG_STACKPROTECTOR_PER_TASK") {
		return nil, nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || fields[0] != "#define" || fields[1] != "TSK_STACK_CANARY" {
			continue
		}
		offset, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("invalid TSK_STACK_CANARY offset %q: %w", fields[2], err)
		}
		return []byte(fmt.Sprintf("-mstack-protector-guard=sysreg\n-mstack-protector-guard-reg=sp_el0\n-mstack-protector-guard-offset=%d\n", offset)), nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("TSK_STACK_CANARY not found")
}

func enabled(config map[string]string, key string) bool {
	value := config[key]
	return value != "" && value != "n"
}

func isCPUCapName(value string) bool {
	for _, r := range value {
		if r == '_' || r == 'v' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return value != ""
}

type sysregGenerator struct {
	out bytes.Buffer

	stack []string

	reg string

	res0 string
	res1 string
	unkn string

	nextBit int
	field   string

	definedRegs   map[string]bool
	definedFields map[string]bool
	seenEnumVals  map[string]bool
}

func generateSysregDefs(data []byte) ([]byte, error) {
	g := &sysregGenerator{
		stack:         []string{"Root"},
		definedRegs:   map[string]bool{},
		definedFields: map[string]bool{},
	}
	g.out.WriteString("#ifndef __ASM_SYSREG_DEFS_H\n")
	g.out.WriteString("#define __ASM_SYSREG_DEFS_H\n\n")
	g.out.WriteString("/* Generated file - do not edit */\n\n")

	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if err := g.handleSysregLine(lineNum, fields); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if g.block() != "Root" {
		return nil, fmt.Errorf("missing terminator for %s block", g.block())
	}
	g.out.WriteString("#endif /* __ASM_SYSREG_DEFS_H */\n")
	return g.out.Bytes(), nil
}

func (g *sysregGenerator) handleSysregLine(lineNum int, fields []string) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("line %d: %s", lineNum, fmt.Sprintf(format, args...))
	}
	expect := func(n int) error {
		if len(fields) != n {
			return fail("%d fields found where %d expected", len(fields), n)
		}
		return nil
	}

	switch fields[0] {
	case "SysregFields":
		if g.block() != "Root" {
			break
		}
		if err := expect(2); err != nil {
			return err
		}
		g.push("SysregFields")
		g.reg = fields[1]
		g.res0, g.res1, g.unkn = "UL(0)", "UL(0)", "UL(0)"
		if g.definedFields[g.reg] {
			return fail("duplicate SysregFields definition for %s", g.reg)
		}
		g.definedFields[g.reg] = true
		g.nextBit = 63
		return nil
	case "EndSysregFields":
		if g.block() != "SysregFields" {
			break
		}
		if err := expect(1); err != nil {
			return err
		}
		if g.nextBit > 0 {
			return fail("unspecified bits in %s", g.reg)
		}
		g.define(g.reg+"_RES0", "("+g.res0+")")
		g.define(g.reg+"_RES1", "("+g.res1+")")
		g.define(g.reg+"_UNKN", "("+g.unkn+")")
		g.out.WriteByte('\n')
		g.reg, g.res0, g.res1, g.unkn = "", "", "", ""
		g.pop()
		return nil
	case "Sysreg":
		if g.block() != "Root" {
			break
		}
		if err := expect(7); err != nil {
			return err
		}
		g.push("Sysreg")
		g.reg = fields[1]
		op0, op1, crn, crm, op2 := fields[2], fields[3], fields[4], fields[5], fields[6]
		g.res0, g.res1, g.unkn = "UL(0)", "UL(0)", "UL(0)"
		if g.definedRegs[g.reg] {
			return fail("duplicate Sysreg definition for %s", g.reg)
		}
		g.definedRegs[g.reg] = true
		g.define("REG_"+g.reg, "S"+op0+"_"+op1+"_C"+crn+"_C"+crm+"_"+op2)
		g.define("SYS_"+g.reg, "sys_reg("+op0+", "+op1+", "+crn+", "+crm+", "+op2+")")
		g.define("SYS_"+g.reg+"_Op0", op0)
		g.define("SYS_"+g.reg+"_Op1", op1)
		g.define("SYS_"+g.reg+"_CRn", crn)
		g.define("SYS_"+g.reg+"_CRm", crm)
		g.define("SYS_"+g.reg+"_Op2", op2)
		g.out.WriteByte('\n')
		g.nextBit = 63
		return nil
	case "EndSysreg":
		if g.block() != "Sysreg" {
			break
		}
		if err := expect(1); err != nil {
			return err
		}
		if g.nextBit > 0 {
			return fail("unspecified bits in %s", g.reg)
		}
		if g.res0 != "" {
			g.define(g.reg+"_RES0", "("+g.res0+")")
		}
		if g.res1 != "" {
			g.define(g.reg+"_RES1", "("+g.res1+")")
		}
		if g.unkn != "" {
			g.define(g.reg+"_UNKN", "("+g.unkn+")")
		}
		if g.res0 != "" || g.res1 != "" || g.unkn != "" {
			g.out.WriteByte('\n')
		}
		g.reg, g.res0, g.res1, g.unkn = "", "", "", ""
		g.pop()
		return nil
	case "Fields", "Mapping":
		if g.block() != "Sysreg" {
			break
		}
		if err := expect(2); err != nil {
			return err
		}
		if g.nextBit != 63 {
			return fail("some fields already defined for %s", g.reg)
		}
		fmt.Fprintf(&g.out, "/* For %s fields see %s */\n\n", g.reg, fields[1])
		g.nextBit = 0
		g.res0, g.res1, g.unkn = "", "", ""
		return nil
	case "Res0", "Res1", "Unkn", "Raz":
		if g.block() != "Sysreg" && g.block() != "SysregFields" {
			break
		}
		if err := expect(2); err != nil {
			return err
		}
		msb, lsb, err := g.parseBitdef(g.reg, fields[0], fields[1])
		if err != nil {
			return fail("%v", err)
		}
		switch fields[0] {
		case "Res0":
			g.res0 += fmt.Sprintf(" | GENMASK_ULL(%d, %d)", msb, lsb)
		case "Res1":
			g.res1 += fmt.Sprintf(" | GENMASK_ULL(%d, %d)", msb, lsb)
		case "Unkn":
			g.unkn += fmt.Sprintf(" | GENMASK_ULL(%d, %d)", msb, lsb)
		}
		return nil
	case "Field", "Enum", "SignedEnum", "UnsignedEnum":
		if g.block() != "Sysreg" && g.block() != "SysregFields" {
			break
		}
		if err := expect(3); err != nil {
			return err
		}
		g.field = fields[2]
		msb, lsb, err := g.parseBitdef(g.reg, g.field, fields[1])
		if err != nil {
			return fail("%v", err)
		}
		g.defineField(g.reg, g.field, msb, lsb)
		switch fields[0] {
		case "Field":
			g.out.WriteByte('\n')
		case "SignedEnum":
			g.define(g.reg+"_"+g.field+"_SIGNED", "true")
			g.push("Enum")
			g.seenEnumVals = map[string]bool{}
		case "UnsignedEnum":
			g.define(g.reg+"_"+g.field+"_SIGNED", "false")
			g.push("Enum")
			g.seenEnumVals = map[string]bool{}
		case "Enum":
			g.push("Enum")
			g.seenEnumVals = map[string]bool{}
		}
		return nil
	case "EndEnum":
		if g.block() != "Enum" {
			break
		}
		if err := expect(1); err != nil {
			return err
		}
		g.field = ""
		g.out.WriteByte('\n')
		g.seenEnumVals = nil
		g.pop()
		return nil
	default:
		if strings.HasPrefix(fields[0], "0b") && g.block() == "Enum" {
			if err := expect(2); err != nil {
				return err
			}
			if g.seenEnumVals[fields[0]] {
				return fail("duplicate Enum value %s for %s", fields[0], fields[1])
			}
			g.seenEnumVals[fields[0]] = true
			g.define(g.reg+"_"+g.field+"_"+fields[1], "UL("+fields[0]+")")
			return nil
		}
	}
	return fail("unhandled statement %q in %s block", strings.Join(fields, " "), g.block())
}

func (g *sysregGenerator) block() string {
	return g.stack[len(g.stack)-1]
}

func (g *sysregGenerator) push(block string) {
	g.stack = append(g.stack, block)
}

func (g *sysregGenerator) pop() {
	g.stack = g.stack[:len(g.stack)-1]
}

func (g *sysregGenerator) define(name, value string) {
	macro := "#define " + name
	if len(macro) >= 56 {
		fmt.Fprintf(&g.out, "%s %s\n", macro, value)
		return
	}
	fmt.Fprintf(&g.out, "%-56s%s\n", macro, value)
}

func (g *sysregGenerator) defineField(reg, field string, msb, lsb int) {
	g.define(reg+"_"+field, fmt.Sprintf("GENMASK(%d, %d)", msb, lsb))
	g.define(reg+"_"+field+"_MASK", fmt.Sprintf("GENMASK(%d, %d)", msb, lsb))
	g.define(reg+"_"+field+"_SHIFT", strconv.Itoa(lsb))
	g.define(reg+"_"+field+"_WIDTH", strconv.Itoa(msb-lsb+1))
}

func (g *sysregGenerator) parseBitdef(reg, field, bitdef string) (int, int, error) {
	msb, lsb, err := parseBitRange(bitdef)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid bit-range definition %q", bitdef)
	}
	if msb != g.nextBit {
		return 0, 0, fmt.Errorf("%s.%s starts at %d not %d", reg, field, msb, g.nextBit)
	}
	if msb > 63 || msb < 0 {
		return 0, 0, fmt.Errorf("%s.%s invalid high bit in %q", reg, field, bitdef)
	}
	if lsb > 63 || lsb < 0 {
		return 0, 0, fmt.Errorf("%s.%s invalid low bit in %q", reg, field, bitdef)
	}
	if msb < lsb {
		return 0, 0, fmt.Errorf("%s.%s invalid bit-range %q", reg, field, bitdef)
	}
	g.nextBit = lsb - 1
	return msb, lsb, nil
}

func parseBitRange(bitdef string) (int, int, error) {
	parts := strings.Split(bitdef, ":")
	switch len(parts) {
	case 1:
		bit, err := strconv.Atoi(parts[0])
		return bit, bit, err
	case 2:
		msb, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, 0, err
		}
		lsb, err := strconv.Atoi(parts[1])
		return msb, lsb, err
	default:
		return 0, 0, fmt.Errorf("invalid bit range")
	}
}

func generateVDSOOffsets(data []byte) ([]byte, error) {
	var out bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || len(fields[1]) != 1 || !strings.HasPrefix(fields[2], "VDSO_") {
			continue
		}
		addr := normalizeVDSOAddress(fields[0])
		name := strings.TrimPrefix(fields[2], "VDSO_")
		fmt.Fprintf(&out, "#define vdso_offset_%s 0x%s\n", name, addr)
	}
	return out.Bytes(), scanner.Err()
}

func normalizeVDSOAddress(addr string) string {
	for len(addr) > 1 && addr[0] == '0' && addr[1] == '0' {
		addr = addr[1:]
	}
	return addr
}
