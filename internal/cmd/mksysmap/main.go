// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	in := flag.String("in", "", "llvm-nm -n input")
	out := flag.String("out", "", "System.map output")
	flag.Parse()

	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-in and -out are required")
		os.Exit(2)
	}
	if err := run(*in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "mksysmap: %v\n", err)
		os.Exit(1)
	}
}

func run(inPath, outPath string) error {
	in, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()

	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		line := scanner.Text()
		if keepSymbolLine(line) {
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func keepSymbolLine(line string) bool {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false
	}
	typ := fields[len(fields)-2]
	name := fields[len(fields)-1]
	if len(typ) != 1 {
		return false
	}
	switch typ[0] {
	case 'a', 'N', 'U', 'w':
		return false
	}

	if strings.Contains(name, "$") || strings.Contains(name, ".L") {
		return false
	}
	if strings.HasPrefix(name, "__efistub_") ||
		strings.HasPrefix(name, "__kcfi_typeid_") ||
		strings.HasPrefix(name, "__kvm_nvhe___kcfi_typeid_") ||
		strings.HasPrefix(name, "__pi___kcfi_typeid_") ||
		strings.HasPrefix(name, "__crc_") ||
		strings.HasPrefix(name, "__kstrtab_") ||
		strings.HasPrefix(name, "__kstrtabns_") ||
		strings.HasPrefix(name, "__mod_device_table__") {
		return false
	}
	if strings.Contains(name, "__") && strings.Contains(name, "Thunk_") {
		return false
	}
	if strings.HasSuffix(name, "_from_arm") ||
		strings.HasSuffix(name, "_from_thumb") ||
		strings.HasSuffix(name, "_veneer") {
		return false
	}
	switch name {
	case "L0", "_SDA_BASE_", "_SDA2_BASE_":
		return false
	}
	if strings.HasPrefix(name, "__UNIQUE_ID_modinfo") {
		return false
	}
	if strings.Contains(name, ".long_branch.") || strings.Contains(name, ".plt_branch.") {
		return false
	}
	return true
}
