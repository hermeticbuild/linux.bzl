// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const (
	crc32PolyLE  = uint32(0xedb88320)
	crc32PolyBE  = uint32(0x04c11db7)
	crc32cPolyLE = uint32(0x82f63b78)

	crc64ECMAPoly = uint64(0x42F0E1EBA9EA3693)
	crc64NVMEPoly = uint64(0x9A6C9329AC4BC9B5)
)

func main() {
	kind := flag.String("kind", "", "Table kind: crc32 or crc64")
	out := flag.String("out", "", "Generated table header output")
	flag.Parse()

	if *kind == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-kind and -out are required")
		os.Exit(2)
	}
	if err := run(*kind, *out); err != nil {
		fmt.Fprintf(os.Stderr, "crctables: %v\n", err)
		os.Exit(1)
	}
}

func run(kind, out string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	output, err := os.Create(out)
	if err != nil {
		return err
	}
	defer output.Close()

	switch kind {
	case "crc32":
		writeCRC32(output)
	case "crc64":
		writeCRC64(output)
	default:
		return fmt.Errorf("unknown -kind %q", kind)
	}
	return nil
}

func writeCRC32(output *os.File) {
	fmt.Fprint(output, "/* this file is generated - do not edit */\n\n")

	tableLE := crc32TableLE(crc32PolyLE)
	fmt.Fprint(output, "static const u32 ____cacheline_aligned crc32table_le[256] = {\n")
	outputCRC32Table(output, tableLE)
	fmt.Fprint(output, "};\n")

	tableBE := crc32TableBE(crc32PolyBE)
	fmt.Fprint(output, "static const u32 ____cacheline_aligned crc32table_be[256] = {\n")
	outputCRC32Table(output, tableBE)
	fmt.Fprint(output, "};\n")

	tableCLE := crc32TableLE(crc32cPolyLE)
	fmt.Fprint(output, "static const u32 ____cacheline_aligned crc32ctable_le[256] = {\n")
	outputCRC32Table(output, tableCLE)
	fmt.Fprint(output, "};\n")
}

func crc32TableLE(polynomial uint32) [256]uint32 {
	var tab [256]uint32
	crc := uint32(1)
	for i := uint32(128); i != 0; i >>= 1 {
		crc = (crc >> 1) ^ selectCRC32Polynomial(crc&1 != 0, polynomial)
		for j := uint32(0); j < 256; j += 2 * i {
			tab[i+j] = crc ^ tab[j]
		}
	}
	return tab
}

func crc32TableBE(polynomial uint32) [256]uint32 {
	var tab [256]uint32
	crc := uint32(0x80000000)
	for i := uint32(1); i < 256; i <<= 1 {
		crc = (crc << 1) ^ selectCRC32Polynomial(crc&0x80000000 != 0, polynomial)
		for j := uint32(0); j < i; j++ {
			tab[i+j] = crc ^ tab[j]
		}
	}
	return tab
}

func selectCRC32Polynomial(enabled bool, polynomial uint32) uint32 {
	if enabled {
		return polynomial
	}
	return 0
}

func outputCRC32Table(output *os.File, table [256]uint32) {
	for i := 0; i < 256; i += 4 {
		fmt.Fprintf(output, "\t0x%08x, 0x%08x, 0x%08x, 0x%08x,\n", table[i], table[i+1], table[i+2], table[i+3])
	}
}

func writeCRC64(output *os.File) {
	fmt.Fprint(output, "/* this file is generated - do not edit */\n\n")
	fmt.Fprint(output, "#include <linux/types.h>\n")
	fmt.Fprint(output, "#include <linux/cache.h>\n\n")

	table := crc64Table(crc64ECMAPoly)
	fmt.Fprint(output, "static const u64 ____cacheline_aligned crc64table[256] = {\n")
	outputCRC64Table(output, table)

	nvmeTable := reflectedCRC64Table(crc64NVMEPoly)
	fmt.Fprint(output, "\nstatic const u64 ____cacheline_aligned crc64nvmetable[256] = {\n")
	outputCRC64Table(output, nvmeTable)
}

func reflectedCRC64Table(poly uint64) [256]uint64 {
	var table [256]uint64
	for i := uint64(0); i < 256; i++ {
		crc := uint64(0)
		c := i
		for j := uint64(0); j < 8; j++ {
			if ((crc ^ (c >> j)) & 1) != 0 {
				crc = (crc >> 1) ^ poly
			} else {
				crc >>= 1
			}
		}
		table[i] = crc
	}
	return table
}

func crc64Table(poly uint64) [256]uint64 {
	var table [256]uint64
	for i := uint64(0); i < 256; i++ {
		crc := uint64(0)
		c := i << 56
		for j := uint64(0); j < 8; j++ {
			if ((crc ^ c) & 0x8000000000000000) != 0 {
				crc = (crc << 1) ^ poly
			} else {
				crc <<= 1
			}
			c <<= 1
		}
		table[i] = crc
	}
	return table
}

func outputCRC64Table(output *os.File, table [256]uint64) {
	for i := 0; i < 256; i++ {
		fmt.Fprintf(output, "\t0x%016xULL", table[i])
		if i&1 != 0 {
			fmt.Fprint(output, ",\n")
		} else {
			fmt.Fprint(output, ", ")
		}
	}
	fmt.Fprint(output, "};\n")
}
