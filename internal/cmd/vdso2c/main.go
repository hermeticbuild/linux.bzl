// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"debug/elf"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var knownSymbols = []string{
	"vvar_start",
	"vvar_page",
	"pvclock_page",
	"hvclock_page",
	"timens_page",
	"VDSO32_NOTE_MASK",
	"__kernel_vsyscall",
	"__kernel_sigreturn",
	"__kernel_rt_sigreturn",
	"int80_landing_pad",
	"vdso32_rt_sigreturn_landing_pad",
	"vdso32_sigreturn_landing_pad",
}

func main() {
	raw := flag.String("raw", "", "unstripped vDSO shared object")
	stripped := flag.String("stripped", "", "stripped vDSO shared object")
	vdsoHeader := flag.String("vdso-header", "", "kernel asm/vdso.h")
	out := flag.String("out", "", "generated C output")
	flag.Parse()
	if *raw == "" || *stripped == "" || *vdsoHeader == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-raw, -stripped, -vdso-header, and -out are required")
		os.Exit(2)
	}
	if err := run(*raw, *stripped, *vdsoHeader, *out); err != nil {
		fmt.Fprintf(os.Stderr, "vdso2c: %v\n", err)
		os.Exit(1)
	}
}

func run(rawPath, strippedPath, vdsoHeaderPath, outPath string) error {
	rawData, err := os.ReadFile(rawPath)
	if err != nil {
		return err
	}
	strippedData, err := os.ReadFile(strippedPath)
	if err != nil {
		return err
	}
	vdsoHeader, err := os.ReadFile(vdsoHeaderPath)
	if err != nil {
		return err
	}
	symbolFields, err := vdsoSymbolFields(vdsoHeader)
	if err != nil {
		return fmt.Errorf("%s: %w", vdsoHeaderPath, err)
	}
	file, err := elf.NewFile(bytes.NewReader(rawData))
	if err != nil {
		return err
	}
	defer file.Close()
	if file.Type != elf.ET_DYN {
		return fmt.Errorf("%s: input is not a shared object", rawPath)
	}
	loadSize, err := validateLoad(file)
	if err != nil {
		return err
	}
	if uint64(len(strippedData)) < loadSize {
		return fmt.Errorf("%s: stripped input is shorter than PT_LOAD segment", strippedPath)
	}
	if err := validateDynamicRelocations(file); err != nil {
		return err
	}
	symbols, err := vdsoSymbols(file)
	if err != nil {
		return err
	}
	if err := validateVvarSymbols(symbols, symbolFields); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	var out strings.Builder
	writeC(&out, rawData, strippedData, file, imageName(outPath), symbols, symbolFields)
	return os.WriteFile(outPath, []byte(out.String()), 0o644)
}

func validateLoad(file *elf.File) (uint64, error) {
	var load *elf.Prog
	for _, prog := range file.Progs {
		switch prog.Type {
		case elf.PT_LOAD:
			if load != nil {
				return 0, fmt.Errorf("multiple PT_LOAD segments")
			}
			if prog.Off != 0 || prog.Vaddr != 0 {
				return 0, fmt.Errorf("PT_LOAD segment is not based at file offset and vaddr zero")
			}
			if prog.Memsz != prog.Filesz {
				return 0, fmt.Errorf("PT_LOAD memsz differs from filesz")
			}
			load = prog
		}
	}
	if load == nil {
		return 0, fmt.Errorf("no PT_LOAD segment")
	}
	return load.Memsz, nil
}

func validateDynamicRelocations(file *elf.File) error {
	dyn := file.Section(".dynamic")
	if dyn == nil {
		return fmt.Errorf("input has no dynamic section")
	}
	data, err := dyn.Data()
	if err != nil {
		return err
	}
	order := file.ByteOrder
	entrySize := 16
	if file.Class == elf.ELFCLASS32 {
		entrySize = 8
	}
	for len(data) >= entrySize {
		var tag uint64
		if file.Class == elf.ELFCLASS32 {
			tag = uint64(order.Uint32(data[:4]))
		} else {
			tag = order.Uint64(data[:8])
		}
		if tag == uint64(elf.DT_NULL) {
			return nil
		}
		switch elf.DynTag(tag) {
		case elf.DT_REL, elf.DT_RELSZ, elf.DT_RELA, elf.DT_RELENT, elf.DT_TEXTREL:
			return fmt.Errorf("vDSO image contains dynamic relocations")
		}
		data = data[entrySize:]
	}
	return nil
}

func vdsoSymbols(file *elf.File) (map[string]int64, error) {
	raw, err := file.Symbols()
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, name := range knownSymbols {
		wanted[name] = true
	}
	out := map[string]int64{}
	for _, sym := range raw {
		if wanted[sym.Name] && sym.Value != 0 {
			out[sym.Name] = int64(sym.Value)
		}
	}
	return out, nil
}

var vdsoSymbolFieldPattern = regexp.MustCompile(`\bsym_([A-Za-z0-9_]+)\s*;`)

func vdsoSymbolFields(header []byte) (map[string]bool, error) {
	known := map[string]bool{}
	for _, name := range knownSymbols {
		known[name] = true
	}
	fields := map[string]bool{}
	for _, match := range vdsoSymbolFieldPattern.FindAllSubmatch(header, -1) {
		name := string(match[1])
		if known[name] {
			fields[name] = true
		}
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("struct vdso_image has no recognized symbol fields")
	}
	return fields, nil
}

func validateVvarSymbols(symbols map[string]int64, fields map[string]bool) error {
	if !fields["vvar_start"] {
		return nil
	}
	vvarStart := symbols["vvar_start"]
	if vvarStart == 0 {
		return fmt.Errorf("vDSO image has no vvar_start symbol")
	}
	if vvarStart%4096 != 0 {
		return fmt.Errorf("vvar_start must be a multiple of 4096")
	}
	for _, name := range []string{"vvar_page", "pvclock_page", "hvclock_page", "timens_page"} {
		value := symbols[name]
		if value == 0 {
			continue
		}
		if value%4096 != 0 {
			return fmt.Errorf("%s must be a multiple of 4096", name)
		}
		if value+4096 < vvarStart {
			return fmt.Errorf("%s underruns vvar_start", name)
		}
		if value+4096 > 0 {
			return fmt.Errorf("%s is on the wrong side of the vDSO text", name)
		}
	}
	return nil
}

func imageName(path string) string {
	name := filepath.Base(path)
	if idx := strings.IndexByte(name, '.'); idx >= 0 {
		name = name[:idx]
	}
	return strings.ReplaceAll(name, "-", "_")
}

func writeC(out *strings.Builder, rawData, strippedData []byte, file *elf.File, name string, symbols map[string]int64, symbolFields map[string]bool) {
	mappingSize := (len(strippedData) + 4095) / 4096 * 4096
	out.WriteString("/* AUTOMATICALLY GENERATED -- DO NOT EDIT */\n\n")
	out.WriteString("#include <linux/linkage.h>\n")
	out.WriteString("#include <linux/init.h>\n")
	out.WriteString("#include <asm/page_types.h>\n")
	out.WriteString("#include <asm/vdso.h>\n\n")
	fmt.Fprintf(out, "static unsigned char raw_data[%d] __ro_after_init __aligned(PAGE_SIZE) = {", mappingSize)
	writeByteArray(out, strippedData)
	out.WriteString("\n};\n\n")

	extable := file.Section("__ex_table")
	if extable != nil {
		fmt.Fprintf(out, "static const unsigned char extable[%d] = {", extable.Size)
		writeByteArray(out, rawData[extable.Offset:extable.Offset+extable.Size])
		out.WriteString("\n};\n\n")
	}

	fmt.Fprintf(out, "const struct vdso_image %s = {\n", name)
	out.WriteString("\t.data = raw_data,\n")
	fmt.Fprintf(out, "\t.size = %d,\n", mappingSize)
	if alt := file.Section(".altinstructions"); alt != nil {
		fmt.Fprintf(out, "\t.alt = %d,\n", alt.Offset)
		fmt.Fprintf(out, "\t.alt_len = %d,\n", alt.Size)
	}
	if extable != nil {
		fmt.Fprintf(out, "\t.extable_base = %d,\n", extable.Offset)
		fmt.Fprintf(out, "\t.extable_len = %d,\n", extable.Size)
		out.WriteString("\t.extable = extable,\n")
	}
	for _, sym := range knownSymbols {
		if value := symbols[sym]; value != 0 && symbolFields[sym] {
			fmt.Fprintf(out, "\t.sym_%s = %d,\n", sym, value)
		}
	}
	out.WriteString("};\n\n")
	fmt.Fprintf(out, "static __init int init_%s(void) {\n", name)
	fmt.Fprintf(out, "\treturn init_vdso_image(&%s);\n", name)
	out.WriteString("};\n")
	fmt.Fprintf(out, "subsys_initcall(init_%s);\n", name)
}

func writeByteArray(out *strings.Builder, data []byte) {
	for i, value := range data {
		if i%10 == 0 {
			out.WriteString("\n\t")
		}
		fmt.Fprintf(out, "0x%02X, ", value)
	}
}
