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
	out := flag.String("out", "", "Generated syscall header output")
	abis := flag.String("abis", "", "Comma-separated ABI filter")
	emitNR := flag.Bool("emit-nr", false, "Emit __NR_syscalls")
	offset := flag.String("offset", "", "Offset expression for syscall numbers")
	prefix := flag.String("prefix", "", "Macro prefix")
	flag.Parse()

	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-in and -out are required")
		os.Exit(2)
	}
	if err := run(*in, *out, *abis, *emitNR, *offset, *prefix); err != nil {
		fmt.Fprintf(os.Stderr, "syscallhdr: %v\n", err)
		os.Exit(1)
	}
}

func run(in, out, abis string, emitNR bool, offset, prefix string) error {
	file, err := os.Open(in)
	if err != nil {
		return err
	}
	defer file.Close()

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	outFile, err := os.Create(out)
	if err != nil {
		return err
	}
	defer outFile.Close()

	allowed := parseABIs(abis)
	guard := headerGuard(filepath.Base(out))
	fmt.Fprintf(outFile, "#ifndef %s\n#define %s\n\n", guard, guard)

	max := int64(-1)
	scanner := bufio.NewScanner(file)
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
			return fmt.Errorf("%s: invalid syscall number %q: %w", in, fields[0], err)
		}
		max = nr
		value := fields[0]
		if offset != "" {
			value = "(" + offset + " + " + value + ")"
		}
		fmt.Fprintf(outFile, "#define __NR_%s%s %s\n", prefix, fields[2], value)
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	if emitNR {
		fmt.Fprintf(outFile, "\n#ifdef __KERNEL__\n#define __NR_%ssyscalls %d\n#endif\n", prefix, max+1)
	}
	fmt.Fprintf(outFile, "\n#endif /* %s */\n", guard)
	return nil
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

func headerGuard(base string) string {
	var b strings.Builder
	b.WriteString("_UAPI_ASM_")
	lastUnderscore := false
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
			lastUnderscore = false
		case r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}
