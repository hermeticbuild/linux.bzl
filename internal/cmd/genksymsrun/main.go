package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const exportSymbolPrefix = " __export_symbol_"

type config struct {
	mode         string
	nm           string
	object       string
	compiler     string
	genksyms     string
	reference    string
	out          string
	linuxVersion string
	compilerArgs []string
}

type toolExecutor interface {
	nm(path, object string) ([]byte, error)
	generate(compiler string, compilerArgs []string, compilerInput io.Reader, genksyms string, genksymsArgs []string, output io.Writer) error
}

type osToolExecutor struct{}

func parseArgs(args []string) (config, error) {
	var cfg config
	flags := flag.NewFlagSet("genksymsrun", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.mode, "mode", "c", "input mode: c or asm")
	flags.StringVar(&cfg.nm, "nm", "", "llvm-nm executable")
	flags.StringVar(&cfg.object, "object", "", "compiled Linux object")
	flags.StringVar(&cfg.compiler, "compiler", "", "target C compiler")
	flags.StringVar(&cfg.genksyms, "genksyms", "", "source-specific genksyms executable")
	flags.StringVar(&cfg.reference, "reference", "", "optional genksyms reference file")
	flags.StringVar(&cfg.out, "out", "", "output modpost command data")
	flags.StringVar(&cfg.linuxVersion, "linux-version", "", "Linux source version (required for asm mode)")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	cfg.compilerArgs = flags.Args()

	var missing []string
	for _, item := range []struct {
		name  string
		value string
	}{
		{name: "-compiler", value: cfg.compiler},
		{name: "-genksyms", value: cfg.genksyms},
		{name: "-nm", value: cfg.nm},
		{name: "-object", value: cfg.object},
		{name: "-out", value: cfg.out},
	} {
		if item.value == "" {
			missing = append(missing, item.name)
		}
	}
	if len(missing) != 0 {
		return config{}, fmt.Errorf("required flags are missing: %s", strings.Join(missing, ", "))
	}
	if cfg.mode != "c" && cfg.mode != "asm" {
		return config{}, fmt.Errorf("-mode must be c or asm, got %q", cfg.mode)
	}
	if cfg.mode == "asm" && cfg.linuxVersion == "" {
		return config{}, errors.New("-linux-version is required in asm mode")
	}
	if len(cfg.compilerArgs) == 0 {
		return config{}, errors.New("compiler arguments are required after --")
	}
	return cfg, nil
}

func exportedSymbols(output []byte) []string {
	var symbols []string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		index := strings.LastIndex(line, exportSymbolPrefix)
		if index < 0 {
			continue
		}
		name := strings.TrimSpace(line[index+len(exportSymbolPrefix):])
		if name != "" {
			symbols = append(symbols, name)
		}
	}
	return symbols
}

func linuxVersionAtLeast(version string, major, minor int) (bool, error) {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false, fmt.Errorf("invalid Linux version %q", version)
	}
	gotMajor, err := strconv.Atoi(parts[0])
	if err != nil {
		return false, fmt.Errorf("invalid Linux version %q", version)
	}
	gotMinor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false, fmt.Errorf("invalid Linux version %q", version)
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor), nil
}

func assemblyInput(version string, symbols []string) (string, error) {
	includeString, err := linuxVersionAtLeast(version, 6, 18)
	if err != nil {
		return "", err
	}
	var source strings.Builder
	source.WriteString("#include <linux/kernel.h>\n")
	if includeString {
		source.WriteString("#include <linux/string.h>\n")
	}
	source.WriteString("#include <asm/asm-prototypes.h>\n")
	for _, symbol := range symbols {
		fmt.Fprintf(&source, "EXPORT_SYMBOL(%s);\n", symbol)
	}
	return source.String(), nil
}

func (osToolExecutor) nm(path, object string) ([]byte, error) {
	cmd := exec.Command(path, object)
	cmd.Env = []string{}
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) != 0 {
			return nil, fmt.Errorf("llvm-nm: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("llvm-nm: %w", err)
	}
	return output, nil
}

func (osToolExecutor) generate(compiler string, compilerArgs []string, compilerInput io.Reader, genksyms string, genksymsArgs []string, output io.Writer) error {
	pipeReader, pipeWriter := io.Pipe()
	defer pipeReader.Close()

	genksymsCmd := exec.Command(genksyms, genksymsArgs...)
	genksymsCmd.Env = []string{}
	genksymsCmd.Stdin = pipeReader
	genksymsCmd.Stdout = output
	genksymsCmd.Stderr = os.Stderr
	if err := genksymsCmd.Start(); err != nil {
		pipeWriter.Close()
		return fmt.Errorf("start genksyms: %w", err)
	}

	compilerCmd := exec.Command(compiler, compilerArgs...)
	compilerCmd.Env = []string{}
	compilerCmd.Stdin = compilerInput
	compilerCmd.Stdout = pipeWriter
	compilerCmd.Stderr = os.Stderr
	if err := compilerCmd.Start(); err != nil {
		pipeWriter.Close()
		_ = genksymsCmd.Wait()
		return fmt.Errorf("start compiler: %w", err)
	}

	compilerErr := compilerCmd.Wait()
	closeErr := pipeWriter.Close()
	genksymsErr := genksymsCmd.Wait()
	if compilerErr != nil || genksymsErr != nil {
		return fmt.Errorf("compiler failed: %v; genksyms failed: %v", compilerErr, genksymsErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close compiler output: %w", closeErr)
	}
	return nil
}

func run(cfg config, executor toolExecutor) error {
	nmOutput, err := executor.nm(cfg.nm, cfg.object)
	if err != nil {
		return err
	}
	symbols := exportedSymbols(nmOutput)

	if err := os.MkdirAll(filepath.Dir(cfg.out), 0o755); err != nil {
		return err
	}
	output, err := os.Create(cfg.out)
	if err != nil {
		return err
	}
	if _, err := output.WriteString("\n"); err != nil {
		output.Close()
		return err
	}

	if len(symbols) != 0 {
		var compilerInput io.Reader
		var genksymsArgs []string
		if cfg.mode == "asm" {
			source, err := assemblyInput(cfg.linuxVersion, symbols)
			if err != nil {
				output.Close()
				return err
			}
			compilerInput = strings.NewReader(source)
		}
		if cfg.reference != "" {
			genksymsArgs = []string{"-r", cfg.reference}
		}
		if err := executor.generate(cfg.compiler, cfg.compilerArgs, compilerInput, cfg.genksyms, genksymsArgs, output); err != nil {
			output.Close()
			return err
		}
	}
	return output.Close()
}

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "genksymsrun: %v\n", err)
		os.Exit(2)
	}
	if err := run(cfg, osToolExecutor{}); err != nil {
		fmt.Fprintf(os.Stderr, "genksymsrun: %v\n", err)
		os.Exit(1)
	}
}
