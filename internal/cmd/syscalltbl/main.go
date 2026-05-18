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
)

func main() {
	in := flag.String("in", "", "Linux syscall table input")
	out := flag.String("out", "", "Generated syscall table header output")
	abis := flag.String("abis", "", "Comma-separated ABI filter")
	flag.Parse()

	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-in and -out are required")
		os.Exit(2)
	}
	if err := run(*in, *out, *abis); err != nil {
		fmt.Fprintf(os.Stderr, "syscalltbl: %v\n", err)
		os.Exit(1)
	}
}

func run(inPath, outPath, abis string) error {
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

	allowed := parseABIs(abis)
	next := int64(0)
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || !abiAllowed(allowed, fields[1]) {
			continue
		}
		nr, err := strconv.ParseInt(fields[0], 0, 64)
		if err != nil {
			return fmt.Errorf("%s: invalid syscall number %q: %w", inPath, fields[0], err)
		}
		if next > nr {
			return fmt.Errorf("%s: syscall table is not sorted or duplicates syscall number %d", inPath, nr)
		}
		for next < nr {
			if _, err := fmt.Fprintf(out, "__SYSCALL(%d, sys_ni_syscall)\n", next); err != nil {
				return err
			}
			next++
		}

		native := ""
		compat := ""
		noreturn := ""
		if len(fields) > 3 {
			native = fields[3]
		}
		if len(fields) > 4 && fields[4] != "-" {
			compat = fields[4]
		}
		if len(fields) > 5 {
			noreturn = fields[5]
			if noreturn != "noreturn" {
				return fmt.Errorf("%s: invalid noreturn marker %q", inPath, noreturn)
			}
		}

		if noreturn != "" && compat != "" {
			fmt.Fprintf(out, "__SYSCALL_COMPAT_NORETURN(%d, %s, %s)\n", nr, native, compat)
		} else if noreturn != "" {
			fmt.Fprintf(out, "__SYSCALL_NORETURN(%d, %s)\n", nr, native)
		} else if compat != "" {
			fmt.Fprintf(out, "__SYSCALL_WITH_COMPAT(%d, %s, %s)\n", nr, native, compat)
		} else if native != "" {
			fmt.Fprintf(out, "__SYSCALL(%d, %s)\n", nr, native)
		} else {
			fmt.Fprintf(out, "__SYSCALL(%d, sys_ni_syscall)\n", nr)
		}
		next = nr + 1
	}
	return scanner.Err()
}

func parseABIs(abis string) map[string]bool {
	out := map[string]bool{}
	for _, abi := range strings.Split(abis, ",") {
		if abi != "" {
			out[abi] = true
		}
	}
	return out
}

func abiAllowed(allowed map[string]bool, abi string) bool {
	return len(allowed) == 0 || allowed[abi]
}
