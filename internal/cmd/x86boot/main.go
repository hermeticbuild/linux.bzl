// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"debug/elf"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	defineRE       = regexp.MustCompile(`^\s*#\s*define\s+([A-Za-z0-9_]+)\b(.*)$`)
	featureRE      = regexp.MustCompile(`^\s*#\s*define\s+X86_FEATURE_([A-Za-z0-9_]+)\s+\(\s*([0-9]+)\s*\*\s*32\s*\+\s*([0-9]+)\s*\)`)
	ncapintsRE     = regexp.MustCompile(`^\s*#\s*define\s+NCAPINTS\s+([0-9]+)\b`)
	requiredMaskRE = regexp.MustCompile(`^\s*#\s*define\s+REQUIRED_MASK([0-9]+)\s+0x([0-9a-fA-F]+)U\b`)
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "append-size":
		err = runAppendSize(os.Args[2:])
	case "bzimage":
		err = runBZImage(os.Args[2:])
	case "concat":
		err = runConcat(os.Args[2:])
	case "cpustr":
		err = runCPUString(os.Args[2:])
	case "empty-dir":
		err = runEmptyDir(os.Args[2:])
	case "offsets":
		err = runOffsets(os.Args[2:])
	case "piggy":
		err = runPiggy(os.Args[2:])
	case "relocs":
		err = runRelocs(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "x86boot %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: x86boot <command> [args]

commands:
  append-size -in <file> [-in <file> ...] [-size-in <file> ...] -out <file>
  bzimage -setup <setup.bin> -kernel <vmlinux.bin> -out <bzImage>
  concat -in <file> [-in <file> ...] -out <file>
  cpustr -cpufeatures <cpufeatures.h> -masks <cpufeaturemasks.h> -out <cpustr.h>
  empty-dir -out <directory>
  offsets -kind <voffset|zoffset> -in <elf> -out <header>
  piggy -in <compressed-with-size> -out <piggy.S>
  relocs -tool <relocs> -in <vmlinux> -out <vmlinux.relocs>`)
}

func runEmptyDir(args []string) error {
	fs := flag.NewFlagSet("empty-dir", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	return os.MkdirAll(*out, 0o755)
}

func runRelocs(args []string) error {
	fs := flag.NewFlagSet("relocs", flag.ContinueOnError)
	tool := fs.String("tool", "", "relocs executable")
	in := fs.String("in", "", "input vmlinux")
	out := fs.String("out", "", "output relocation data")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tool == "" || *in == "" || *out == "" {
		return fmt.Errorf("-tool, -in, and -out are required")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	output, err := os.Create(*out)
	if err != nil {
		return err
	}
	cmd := exec.Command(*tool, *in)
	cmd.Env = []string{}
	cmd.Stdout = output
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	closeErr := output.Close()
	if runErr != nil {
		return runErr
	}
	if closeErr != nil {
		return closeErr
	}
	validate := exec.Command(*tool, "--abs-relocs", *in)
	validate.Env = []string{}
	validate.Stdout = os.Stdout
	validate.Stderr = os.Stderr
	return validate.Run()
}

type repeatedFlag []string

func (f *repeatedFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func runAppendSize(args []string) error {
	fs := flag.NewFlagSet("append-size", flag.ContinueOnError)
	var inputs repeatedFlag
	var sizeInputs repeatedFlag
	out := fs.String("out", "", "output file")
	fs.Var(&inputs, "in", "input file")
	fs.Var(&sizeInputs, "size-in", "input file included in the appended uncompressed size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(inputs) == 0 || *out == "" {
		return fmt.Errorf("-in and -out are required")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	output, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer output.Close()

	for _, input := range inputs {
		file, err := os.Open(input)
		if err != nil {
			return err
		}
		if _, err := io.Copy(output, file); err != nil {
			file.Close()
			return err
		}
		file.Close()
	}
	totalInputs := []string(inputs)
	if len(sizeInputs) != 0 {
		totalInputs = []string(sizeInputs)
	}
	total, err := fileSizeTotal(totalInputs)
	if err != nil {
		return err
	}
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], total)
	_, err = output.Write(size[:])
	return err
}

func runConcat(args []string) error {
	fs := flag.NewFlagSet("concat", flag.ContinueOnError)
	var inputs repeatedFlag
	out := fs.String("out", "", "output file")
	fs.Var(&inputs, "in", "input file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(inputs) == 0 || *out == "" {
		return fmt.Errorf("-in and -out are required")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	output, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer output.Close()
	for _, input := range inputs {
		if err := copyFile(output, input); err != nil {
			return err
		}
	}
	return nil
}

func fileSizeTotal(paths []string) (uint32, error) {
	var total uint32
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		if info.Size() < 0 || info.Size() > (1<<32)-1-int64(total) {
			return 0, fmt.Errorf("input size overflow")
		}
		total += uint32(info.Size())
	}
	return total, nil
}

func runBZImage(args []string) error {
	fs := flag.NewFlagSet("bzimage", flag.ContinueOnError)
	setup := fs.String("setup", "", "setup.bin input")
	kernel := fs.String("kernel", "", "vmlinux.bin input")
	out := fs.String("out", "", "bzImage output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *setup == "" || *kernel == "" || *out == "" {
		return fmt.Errorf("-setup, -kernel, and -out are required")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	output, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer output.Close()
	if err := copyPadded(output, *setup, 4096); err != nil {
		return err
	}
	return copyFile(output, *kernel)
}

func copyPadded(w io.Writer, path string, blockSize int64) error {
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	defer input.Close()
	n, err := io.Copy(w, input)
	if err != nil {
		return err
	}
	padding := (blockSize - n%blockSize) % blockSize
	if padding == 0 {
		return nil
	}
	_, err = w.Write(make([]byte, padding))
	return err
}

func copyFile(w io.Writer, path string) error {
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	defer input.Close()
	_, err = io.Copy(w, input)
	return err
}

func runCPUString(args []string) error {
	fs := flag.NewFlagSet("cpustr", flag.ContinueOnError)
	cpufeatures := fs.String("cpufeatures", "", "arch/x86/include/asm/cpufeatures.h input")
	masks := fs.String("masks", "", "generated cpufeaturemasks.h input")
	out := fs.String("out", "", "cpustr.h output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cpufeatures == "" || *masks == "" || *out == "" {
		return fmt.Errorf("-cpufeatures, -masks, and -out are required")
	}
	features, ncapints, err := parseCPUFeatures(*cpufeatures)
	if err != nil {
		return err
	}
	if ncapints == 0 {
		return fmt.Errorf("%s: NCAPINTS not found", *cpufeatures)
	}
	if _, err := parseRequiredMasks(*masks); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("#include <asm/cpufeaturemasks.h>\n\n")
	b.WriteString("static const char x86_cap_strs[] =\n")
	for i := 0; i < ncapints; i++ {
		for j := 0; j < 32; j++ {
			value := features[i*32+j]
			if i == ncapints-1 && j == 31 {
				fmt.Fprintf(&b, "\t\"\\x%02x\\x%02x\"\"%s\"\n", i, j, value)
				continue
			}
			if value == "" {
				continue
			}
			fmt.Fprintf(&b, "#if REQUIRED_MASK%d & (1 << %d)\n", i, j)
			fmt.Fprintf(&b, "\t\"\\x%02x\\x%02x\"\"%s\\0\"\n", i, j, value)
			b.WriteString("#endif\n")
		}
	}
	b.WriteString("\t;\n")
	return os.WriteFile(*out, []byte(b.String()), 0o644)
}

func parseCPUFeatures(path string) (map[int]string, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	features := map[int]string{}
	ncapints := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if match := ncapintsRE.FindStringSubmatch(line); match != nil {
			ncapints, err = strconv.Atoi(match[1])
			if err != nil {
				return nil, 0, err
			}
			continue
		}
		match := featureRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		word, err := strconv.Atoi(match[2])
		if err != nil {
			return nil, 0, err
		}
		bit, err := strconv.Atoi(match[3])
		if err != nil {
			return nil, 0, err
		}
		value := quotedCommentValue(line)
		if value == "" {
			continue
		}
		features[word*32+bit] = strings.ToLower(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}
	return features, ncapints, nil
}

func parseRequiredMasks(path string) (map[int]uint32, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	out := map[int]uint32{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		match := requiredMaskRE.FindStringSubmatch(scanner.Text())
		if match == nil {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, err
		}
		value, err := strconv.ParseUint(match[2], 16, 32)
		if err != nil {
			return nil, err
		}
		out[index] = uint32(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func quotedCommentValue(line string) string {
	match := defineRE.FindStringSubmatch(line)
	if match == nil {
		return ""
	}
	rest := match[2]
	start := strings.Index(rest, "/*")
	end := strings.LastIndex(rest, "*/")
	if start < 0 || end < start {
		return ""
	}
	comment := rest[start+2 : end]
	first := strings.IndexByte(comment, '"')
	if first < 0 {
		return ""
	}
	comment = comment[first+1:]
	second := strings.IndexByte(comment, '"')
	if second < 0 {
		return ""
	}
	return comment[:second]
}

type offsetSymbol struct {
	name  string
	value uint64
}

func runOffsets(args []string) error {
	fs := flag.NewFlagSet("offsets", flag.ContinueOnError)
	kind := fs.String("kind", "", "offset kind: voffset or zoffset")
	in := fs.String("in", "", "ELF input")
	out := fs.String("out", "", "header output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *kind == "" || *in == "" || *out == "" {
		return fmt.Errorf("-kind, -in, and -out are required")
	}
	symbols, err := readELFSymbols(*in)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	switch *kind {
	case "voffset":
		writeOffsets(&b, symbols, "VO_", []string{
			"_text",
			"__start_rodata",
			"_sinittext",
			"__inittext_end",
			"__bss_start",
			"_end",
		}, true)
	case "zoffset":
		writeOffsets(&b, symbols, "ZO_", []string{
			"startup_32",
			"efi32_stub_entry",
			"efi64_stub_entry",
			"efi_pe_entry",
			"efi32_pe_entry",
			"input_data",
			"kernel_info",
			"_end",
			"_ehead",
			"_text",
			"_data",
			"_edata",
			"_sbat",
			"_esbat",
		}, false)
		writePrefixOffsets(&b, symbols, "ZO_", "z_", false)
	default:
		return fmt.Errorf("unsupported offset kind %q", *kind)
	}
	return os.WriteFile(*out, []byte(b.String()), 0o644)
}

func readELFSymbols(path string) ([]offsetSymbol, error) {
	file, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	symbols, err := file.Symbols()
	if err != nil {
		return nil, err
	}
	out := make([]offsetSymbol, 0, len(symbols))
	for _, symbol := range symbols {
		if symbol.Name == "" {
			continue
		}
		out = append(out, offsetSymbol{name: symbol.Name, value: symbol.Value})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].value == out[j].value {
			return out[i].name < out[j].name
		}
		return out[i].value < out[j].value
	})
	return out, nil
}

func writeOffsets(out *strings.Builder, symbols []offsetSymbol, prefix string, names []string, ac bool) {
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	for _, symbol := range symbols {
		if !wanted[symbol.name] {
			continue
		}
		writeOffset(out, prefix, symbol.name, symbol.value, ac)
	}
}

func writePrefixOffsets(out *strings.Builder, symbols []offsetSymbol, definePrefix, symbolPrefix string, ac bool) {
	for _, symbol := range symbols {
		if strings.HasPrefix(symbol.name, symbolPrefix) {
			writeOffset(out, definePrefix, symbol.name, symbol.value, ac)
		}
	}
}

func writeOffset(out *strings.Builder, prefix, name string, value uint64, ac bool) {
	if ac {
		fmt.Fprintf(out, "#define %s%s _AC(0x%x,UL)\n", prefix, name, value)
	} else {
		fmt.Fprintf(out, "#define %s%s 0x%x\n", prefix, name, value)
	}
}

func runPiggy(args []string) error {
	fs := flag.NewFlagSet("piggy", flag.ContinueOnError)
	in := fs.String("in", "", "compressed input with trailing uncompressed size")
	out := fs.String("out", "", "piggy.S output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *out == "" {
		return fmt.Errorf("-in and -out are required")
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	if len(data) < 4 {
		return fmt.Errorf("%s: input is shorter than trailing size", *in)
	}
	uncompressedSize := binary.LittleEndian.Uint32(data[len(data)-4:])
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, ".section \".rodata..compressed\",\"a\",@progbits\n")
	fmt.Fprintf(&b, ".globl z_input_len\n")
	fmt.Fprintf(&b, "z_input_len = %d\n", len(data))
	fmt.Fprintf(&b, ".globl z_output_len\n")
	fmt.Fprintf(&b, "z_output_len = %d\n", uncompressedSize)
	fmt.Fprintf(&b, ".globl input_data, input_data_end\n")
	fmt.Fprintf(&b, "input_data:\n")
	fmt.Fprintf(&b, ".incbin \"%s\"\n", *in)
	fmt.Fprintf(&b, "input_data_end:\n")
	fmt.Fprintf(&b, ".section \".rodata\",\"a\",@progbits\n")
	fmt.Fprintf(&b, ".globl input_len\n")
	fmt.Fprintf(&b, "input_len:\n\t.long %d\n", len(data))
	fmt.Fprintf(&b, ".globl output_len\n")
	fmt.Fprintf(&b, "output_len:\n\t.long %d\n", uncompressedSize)
	return os.WriteFile(*out, []byte(b.String()), 0o644)
}
