// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	input := flag.String("input", "", "Raw .modinfo section extracted from vmlinux")
	modinfoOut := flag.String("modinfo_out", "", "Normalized modules.builtin.modinfo output")
	modulesOut := flag.String("modules_out", "", "modules.builtin output")
	flag.Parse()

	if *input == "" || *modinfoOut == "" || *modulesOut == "" {
		flag.Usage()
		os.Exit(2)
	}
	raw, err := os.ReadFile(*input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read .modinfo: %v\n", err)
		os.Exit(1)
	}
	modinfo, modules := processModinfo(raw)
	if err := os.WriteFile(*modinfoOut, modinfo, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write modules.builtin.modinfo: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*modulesOut, []byte(modules), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write modules.builtin: %v\n", err)
		os.Exit(1)
	}
}

func processModinfo(raw []byte) ([]byte, string) {
	normalized := bytes.TrimRight(raw, "\x00")
	if len(normalized) != 0 {
		normalized = append(normalized, 0)
	}

	var modules strings.Builder
	seen := map[string]bool{}
	for _, record := range bytes.Split(normalized, []byte{0}) {
		value, ok := builtinFileValue(string(record))
		if !ok {
			continue
		}
		for _, path := range strings.Fields(value) {
			module := "kernel/" + path + ".ko"
			if seen[module] {
				continue
			}
			seen[module] = true
			modules.WriteString(module)
			modules.WriteByte('\n')
		}
	}
	return normalized, modules.String()
}

func builtinFileValue(record string) (string, bool) {
	prefix, value, ok := strings.Cut(record, ".file=")
	if !ok {
		return "", false
	}
	for _, r := range prefix {
		if (r < 'a' || r > 'z') &&
			(r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') &&
			r != ':' &&
			r != '_' {
			return "", false
		}
	}
	return value, true
}
