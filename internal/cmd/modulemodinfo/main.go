package main

import (
	"bytes"
	"debug/elf"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	in := flag.String("in", "", "input module ELF")
	out := flag.String("out", "", "validation marker output")
	flag.Parse()

	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-in and -out are required")
		os.Exit(2)
	}
	if err := run(*in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "modulemodinfo: %v\n", err)
		os.Exit(1)
	}
}

func run(inPath, outPath string) error {
	module, err := elf.Open(inPath)
	if err != nil {
		return fmt.Errorf("open module ELF %q: %w", inPath, err)
	}
	defer module.Close()

	if section := module.Section(".modinfo"); section != nil {
		data, err := section.Data()
		if err != nil {
			return fmt.Errorf("read .modinfo from %q: %w", inPath, err)
		}
		if err := checkModuleModinfo(data); err != nil {
			return fmt.Errorf("%s: %w", inPath, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(outPath, []byte("ok\n"), 0o600)
}

func checkModuleModinfo(data []byte) error {
	for _, entry := range bytes.Split(data, []byte{0}) {
		key, value, found := bytes.Cut(entry, []byte{'='})
		if !found {
			continue
		}
		if bytes.Equal(key, []byte("version")) || bytes.Equal(key, []byte("srcversion")) {
			return fmt.Errorf(
				"module source-version metadata is not supported: %s=%s",
				key,
				value,
			)
		}
	}
	return nil
}
