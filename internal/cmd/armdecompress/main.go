package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const armZstdDecompressor = `#ifdef CONFIG_KERNEL_ZSTD
#include "../../../../lib/decompress_unzstd.c"
#endif

`

var armDoDecompress = regexp.MustCompile(`(?m)^[\t ]*int[\t ]+do_decompress[\t ]*\(`)

func main() {
	input := flag.String("in", "", "arch/arm/boot/compressed/decompress.c input")
	output := flag.String("out", "", "adapted ARM decompressor source output")
	flag.Parse()
	if *input == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "-in and -out are required")
		os.Exit(2)
	}

	source, err := os.ReadFile(*input)
	if err != nil {
		fatal("read input", err)
	}
	generated, err := generateARMDecompressor(source)
	if err != nil {
		fatal("generate source", err)
	}
	if err := os.WriteFile(*output, generated, 0o644); err != nil {
		fatal("write output", err)
	}
}

func fatal(action string, err error) {
	fmt.Fprintf(os.Stderr, "armdecompress: %s: %v\n", action, err)
	os.Exit(1)
}

func generateARMDecompressor(source []byte) ([]byte, error) {
	if armDecompressorHasZstd(source) {
		return append([]byte(nil), source...), nil
	}
	location := armDoDecompress.FindIndex(source)
	if location == nil {
		return nil, fmt.Errorf("do_decompress definition not found")
	}

	generated := make([]byte, 0, len(source)+len(armZstdDecompressor))
	generated = append(generated, source[:location[0]]...)
	generated = append(generated, armZstdDecompressor...)
	generated = append(generated, source[location[0]:]...)
	return generated, nil
}

func armDecompressorHasZstd(source []byte) bool {
	for _, rawLine := range bytes.Split(source, []byte("\n")) {
		line := strings.TrimSpace(string(rawLine))
		if !strings.HasPrefix(line, "#") {
			continue
		}
		directive := strings.TrimSpace(strings.TrimPrefix(line, "#"))
		fields := strings.Fields(directive)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "include" && strings.Contains(strings.ToLower(directive), "unzstd") {
			return true
		}
		if fields[0] != "if" && fields[0] != "ifdef" && fields[0] != "elif" {
			continue
		}
		if strings.Contains(directive, "CONFIG_KERNEL_ZSTD") {
			return true
		}
	}
	return false
}
