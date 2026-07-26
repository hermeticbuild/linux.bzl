package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	constHelperPattern = regexp.MustCompile(`pub const RUST_CONST_HELPER_([A-Za-z0-9_]*)`)
	rustHelperPattern  = regexp.MustCompile(`pub fn rust_helper_([A-Za-z0-9_]*)`)
)

func main() {
	in := flag.String("in", "", "input file")
	out := flag.String("out", "", "output file")
	mode := flag.String("mode", "", "transformation: bindings, exports, helpers, or rust-asm")
	flag.Parse()

	if *in == "" || *out == "" || *mode == "" {
		fmt.Fprintln(os.Stderr, "-in, -out, and -mode are required")
		os.Exit(2)
	}
	if err := run(*mode, *in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "rustpostprocess: %v\n", err)
		os.Exit(1)
	}
}

func run(mode, inPath, outPath string) error {
	input, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}

	var output []byte
	switch mode {
	case "bindings":
		output = constHelperPattern.ReplaceAll(input, []byte("pub const $1"))
	case "exports":
		output = exportHeader(input)
	case "helpers":
		output = rustHelperPattern.ReplaceAll(
			input,
			[]byte("#[link_name=\"rust_helper_$1\"]\n    pub fn $1"),
		)
	case "rust-asm":
		output, err = rustAssembly(input)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported mode %q", mode)
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(outPath, output, 0o600)
}

func rustAssembly(input []byte) ([]byte, error) {
	const marker = "// Cut here."
	index := bytes.Index(input, []byte(marker))
	if index < 0 {
		return nil, fmt.Errorf("preprocessed Rust assembly is missing %q", marker)
	}
	output := input[index+len(marker):]
	output = bytes.TrimLeft(output, "\r\n")
	return append(output, '\n'), nil
}

func exportHeader(input []byte) []byte {
	var output strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(input))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		kind := fields[len(fields)-2]
		name := fields[len(fields)-1]
		if len(kind) != 1 || !strings.Contains("TRDB", kind) {
			continue
		}
		if strings.Contains(name, "__pfx") ||
			strings.Contains(name, "__cfi") ||
			strings.Contains(name, "__odr_asan") {
			continue
		}
		fmt.Fprintf(&output, "EXPORT_SYMBOL_RUST_GPL(%s);\n", name)
	}
	return []byte(output.String())
}
