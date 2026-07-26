// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const outputPlaceholder = "{output}"

type repeatedFlag []string

func (f *repeatedFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type options struct {
	inputPath      string
	outputPath     string
	elfETRelEndian string
	environment    []string
	toolPath       string
	toolArguments  []string
	commandStdout  io.Writer
	commandStderr  io.Writer
}

func main() {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "btfmutate: %v\n", err)
		fmt.Fprintln(os.Stderr, "usage: btfmutate -input INPUT -output OUTPUT [-env NAME=VALUE] [-elf-et-rel-endian little|big] -- TOOL ARG...")
		os.Exit(2)
	}
	opts.commandStdout = os.Stdout
	opts.commandStderr = os.Stderr
	if err := run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "btfmutate: %v\n", err)
		os.Exit(1)
	}
}

func parseOptions(arguments []string) (options, error) {
	var opts options
	var environment repeatedFlag
	flags := flag.NewFlagSet("btfmutate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.inputPath, "input", "", "immutable input file")
	flags.StringVar(&opts.outputPath, "output", "", "mutable output file")
	flags.StringVar(&opts.elfETRelEndian, "elf-et-rel-endian", "", "patch ELF e_type to ET_REL using little or big endian")
	flags.Var(&environment, "env", "environment entry for the tool, as NAME=VALUE; may be repeated")
	if err := flags.Parse(arguments); err != nil {
		return options{}, err
	}
	if opts.inputPath == "" {
		return options{}, errors.New("-input is required")
	}
	if opts.outputPath == "" {
		return options{}, errors.New("-output is required")
	}
	switch opts.elfETRelEndian {
	case "", "little", "big":
	default:
		return options{}, fmt.Errorf("-elf-et-rel-endian must be little or big, got %q", opts.elfETRelEndian)
	}

	tool := flags.Args()
	if len(tool) == 0 {
		return options{}, errors.New("TOOL is required after --")
	}
	opts.toolPath = tool[0]
	if !strings.ContainsRune(opts.toolPath, filepath.Separator) {
		return options{}, fmt.Errorf("TOOL must be an explicit path, got %q", opts.toolPath)
	}
	opts.toolArguments = append([]string(nil), tool[1:]...)
	placeholderCount := 0
	for _, argument := range opts.toolArguments {
		placeholderCount += strings.Count(argument, outputPlaceholder)
	}
	if placeholderCount != 1 {
		return options{}, fmt.Errorf("tool arguments must contain %s exactly once, found %d", outputPlaceholder, placeholderCount)
	}
	if err := validateEnvironment(environment); err != nil {
		return options{}, err
	}
	opts.environment = append([]string(nil), environment...)
	return opts, nil
}

func validateEnvironment(environment []string) error {
	seen := map[string]bool{}
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return fmt.Errorf("-env must have the form NAME=VALUE, got %q", entry)
		}
		if strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("invalid -env entry %q", entry)
		}
		if seen[name] {
			return fmt.Errorf("-env %q is specified more than once", name)
		}
		seen[name] = true
	}
	return nil
}

func run(opts options) error {
	toolArguments := make([]string, 0, len(opts.toolArguments))
	for _, argument := range opts.toolArguments {
		toolArguments = append(toolArguments, strings.ReplaceAll(argument, outputPlaceholder, opts.outputPath))
	}
	if err := copyWritable(opts.inputPath, opts.outputPath); err != nil {
		return err
	}

	command := exec.Command(opts.toolPath, toolArguments...)
	command.Env = append([]string(nil), opts.environment...)
	command.Stdout = opts.commandStdout
	command.Stderr = opts.commandStderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("run %q: %w", opts.toolPath, err)
	}
	if opts.elfETRelEndian != "" {
		if err := patchELFTypeToRelocatable(opts.outputPath, opts.elfETRelEndian); err != nil {
			return err
		}
	}
	return nil
}

func copyWritable(inputPath, outputPath string) error {
	inputAbsolute, err := filepath.Abs(inputPath)
	if err != nil {
		return fmt.Errorf("resolve input path %q: %w", inputPath, err)
	}
	outputAbsolute, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolve output path %q: %w", outputPath, err)
	}
	if filepath.Clean(inputAbsolute) == filepath.Clean(outputAbsolute) {
		return errors.New("input and output must be different files")
	}

	input, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("open input %q: %w", inputPath, err)
	}
	defer input.Close()
	inputInfo, err := input.Stat()
	if err != nil {
		return fmt.Errorf("stat input %q: %w", inputPath, err)
	}
	if outputInfo, err := os.Stat(outputPath); err == nil && os.SameFile(inputInfo, outputInfo) {
		return errors.New("input and output must be different files")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat output %q: %w", outputPath, err)
	}

	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open output %q: %w", outputPath, err)
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return fmt.Errorf("copy %q to %q: %w", inputPath, outputPath, err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close output %q: %w", outputPath, err)
	}
	if err := os.Chmod(outputPath, 0o600); err != nil {
		return fmt.Errorf("make output %q writable: %w", outputPath, err)
	}
	return nil
}

func patchELFTypeToRelocatable(path, endian string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open ELF output %q: %w", path, err)
	}
	defer file.Close()

	header := make([]byte, 18)
	if _, err := io.ReadFull(file, header); err != nil {
		return fmt.Errorf("read ELF header from %q: %w", path, err)
	}
	if !bytes.Equal(header[:4], []byte(elf.ELFMAG)) {
		return fmt.Errorf("output %q does not have an ELF header", path)
	}

	var order binary.ByteOrder
	var dataEncoding byte
	switch endian {
	case "little":
		order = binary.LittleEndian
		dataEncoding = byte(elf.ELFDATA2LSB)
	case "big":
		order = binary.BigEndian
		dataEncoding = byte(elf.ELFDATA2MSB)
	default:
		return fmt.Errorf("unsupported ELF byte order %q", endian)
	}
	if header[elf.EI_DATA] != dataEncoding {
		return fmt.Errorf("output %q ELF byte order does not match requested %s endian", path, endian)
	}

	order.PutUint16(header[16:18], uint16(elf.ET_REL))
	if _, err := file.WriteAt(header[16:18], 16); err != nil {
		return fmt.Errorf("patch ELF e_type in %q: %w", path, err)
	}
	return nil
}
