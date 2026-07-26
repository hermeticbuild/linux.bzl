package main

import (
	"bufio"
	"crypto/sha1"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	in := flag.String("in", "", "arch/x86/include/asm/orc_types.h input")
	out := flag.String("out", "", "Generated asm/orc_hash.h output")
	flag.Parse()

	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-in and -out are required")
		os.Exit(2)
	}
	if err := run(*in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "orchash: %v\n", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	input, err := os.Open(in)
	if err != nil {
		return err
	}
	defer input.Close()

	hash := sha1.New()
	scanner := bufio.NewScanner(input)
	inStruct := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#define ORC_REG_") || strings.HasPrefix(line, "#define ORC_TYPE_") {
			fmt.Fprintln(hash, line)
		}
		if line == "struct orc_entry {" {
			inStruct = true
		}
		if inStruct {
			fmt.Fprintln(hash, line)
		}
		if inStruct && strings.HasPrefix(line, "}") {
			inStruct = false
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: %w", in, err)
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	output, err := os.Create(out)
	if err != nil {
		return err
	}
	defer output.Close()

	fmt.Fprint(output, "#define ORC_HASH ")
	for _, b := range hash.Sum(nil) {
		fmt.Fprintf(output, "0x%02x,", b)
	}
	return nil
}
