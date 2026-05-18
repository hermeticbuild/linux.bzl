// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const fontLen = 256

func main() {
	in := flag.String("in", "", "console font unicode map input")
	out := flag.String("out", "", "generated console map C output")
	flag.Parse()
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-in and -out are required")
		os.Exit(2)
	}
	if err := run(*in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "conmakehash: %v\n", err)
		os.Exit(1)
	}
}

func run(inPath, outPath string) error {
	table := make([][]uint16, fontLen)
	file, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		if err := parseLine(table, scanner.Text()); err != nil {
			return fmt.Errorf("%s:%d: %w", inPath, lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(outPath, []byte(render(table)), 0o644)
}

func parseLine(table [][]uint16, line string) error {
	if idx := strings.IndexByte(line, '#'); idx >= 0 {
		line = line[:idx]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return fmt.Errorf("bad input line %q", line)
	}
	fp0, fp1, err := parseFontRange(fields[0])
	if err != nil {
		return err
	}
	if fp0 < 0 || fp0 >= fontLen || fp1 < fp0 || fp1 >= fontLen {
		return fmt.Errorf("font position range %d-%d outside 0-%d", fp0, fp1, fontLen-1)
	}

	if fp1 != fp0 {
		if fields[1] == "idem" {
			for fp := fp0; fp <= fp1; fp++ {
				addPair(table, fp, fp)
			}
			return nil
		}
		un0Text, un1Text, ok := unicodeRangeFields(fields[1:])
		if !ok {
			return fmt.Errorf("font position range requires idem or a unicode range")
		}
		un0, err := parseUnicode(un0Text)
		if err != nil {
			return err
		}
		un1, err := parseUnicode(un1Text)
		if err != nil {
			return err
		}
		return addUnicodeRange(table, fp0, fp1, un0, un1)
	}

	for _, field := range fields[1:] {
		un, err := parseUnicode(field)
		if err != nil {
			return err
		}
		addPair(table, fp0, un)
	}
	return nil
}

func unicodeRangeFields(fields []string) (string, string, bool) {
	if len(fields) == 1 {
		left, right, ok := strings.Cut(fields[0], "-")
		return strings.TrimSpace(left), strings.TrimSpace(right), ok
	}
	if len(fields) == 3 && fields[1] == "-" {
		return fields[0], fields[2], true
	}
	return "", "", false
}

func parseFontRange(field string) (int, int, error) {
	left, right, ok := strings.Cut(field, "-")
	fp0, err := strconv.ParseInt(left, 0, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("bad font position %q", field)
	}
	if !ok {
		return int(fp0), int(fp0), nil
	}
	fp1, err := strconv.ParseInt(right, 0, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("bad font position range %q", field)
	}
	return int(fp0), int(fp1), nil
}

func parseUnicode(field string) (int, error) {
	if len(field) != len("U+0000") || !strings.HasPrefix(field, "U+") {
		return 0, fmt.Errorf("bad unicode value %q", field)
	}
	for _, r := range field[2:] {
		if !unicode.Is(unicode.Hex_Digit, r) {
			return 0, fmt.Errorf("bad unicode value %q", field)
		}
	}
	value, err := strconv.ParseInt(field[2:], 16, 32)
	if err != nil {
		return 0, err
	}
	return int(value), nil
}

func addUnicodeRange(table [][]uint16, fp0, fp1, un0, un1 int) error {
	if un1-un0 != fp1-fp0 {
		return fmt.Errorf("unicode range U+%04x-U+%04x does not match font range %d-%d", un0, un1, fp0, fp1)
	}
	for fp := fp0; fp <= fp1; fp++ {
		addPair(table, fp, un0-fp0+fp)
	}
	return nil
}

func addPair(table [][]uint16, fp int, un int) {
	if un > 0xfffe {
		return
	}
	value := uint16(un)
	for _, existing := range table[fp] {
		if existing == value {
			return
		}
	}
	table[fp] = append(table[fp], value)
}

func render(table [][]uint16) string {
	total := 0
	for _, entries := range table {
		total += len(entries)
	}
	var out strings.Builder
	out.WriteString("/*\n * Automatically generated file; Do not edit.\n */\n\n")
	out.WriteString("#include <linux/types.h>\n\n")
	fmt.Fprintf(&out, "u8 dfont_unicount[%d] = \n{\n\t", fontLen)
	for i, entries := range table {
		fmt.Fprintf(&out, "%3d", len(entries))
		if i == fontLen-1 {
			out.WriteString("\n};\n")
		} else if i%8 == 7 {
			out.WriteString(",\n\t")
		} else {
			out.WriteString(", ")
		}
	}
	fmt.Fprintf(&out, "\nu16 dfont_unitable[%d] = \n{\n\t", total)
	written := 0
	for _, entries := range table {
		for _, value := range entries {
			fmt.Fprintf(&out, "0x%04x", value)
			written++
			if written == total {
				out.WriteString("\n};\n")
			} else if (written-1)%8 == 7 {
				out.WriteString(",\n\t")
			} else {
				out.WriteString(", ")
			}
		}
	}
	return out.String()
}
