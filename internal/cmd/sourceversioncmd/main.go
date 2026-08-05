// sourceversioncmd converts a compiler dependency file into the subset of a
// Kbuild .cmd file consumed by scripts/mod/sumversion.c.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type stringsFlag []string

func (f *stringsFlag) String() string { return strings.Join(*f, ",") }
func (f *stringsFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type config struct {
	depfile     string
	object      string
	out         string
	primary     string
	symversions string
	physical    []string
	canonical   []string
}

func parseArgs(args []string) (config, error) {
	var cfg config
	var physical, canonical stringsFlag
	flags := flag.NewFlagSet("sourceversioncmd", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&cfg.depfile, "depfile", "", "compiler dependency file")
	flags.StringVar(&cfg.object, "object", "", "canonical object path")
	flags.StringVar(&cfg.out, "out", "", "output Kbuild command file")
	flags.StringVar(&cfg.primary, "primary", "", "canonical primary source path")
	flags.StringVar(&cfg.symversions, "symversions", "", "optional genksyms command data")
	flags.Var(&physical, "physical", "physical source path (repeatable)")
	flags.Var(&canonical, "canonical", "canonical source path (repeatable)")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	cfg.physical = physical
	cfg.canonical = canonical
	for _, required := range []struct {
		name, value string
	}{
		{"-depfile", cfg.depfile},
		{"-object", cfg.object},
		{"-out", cfg.out},
		{"-primary", cfg.primary},
	} {
		if required.value == "" {
			return config{}, fmt.Errorf("%s is required", required.name)
		}
	}
	if len(cfg.physical) != len(cfg.canonical) || len(cfg.physical) == 0 {
		return config{}, errors.New("matching non-empty -physical and -canonical mappings are required")
	}
	return cfg, nil
}

func depfileDependencies(data []byte) ([]string, error) {
	escaped := false
	colon := -1
	for index, b := range data {
		if escaped {
			escaped = false
			continue
		}
		if b == '\\' {
			escaped = true
			continue
		}
		if b == ':' {
			colon = index
			break
		}
	}
	if colon < 0 {
		return nil, errors.New("dependency file has no target separator")
	}

	var dependencies []string
	var word strings.Builder
	flush := func() {
		if word.Len() != 0 {
			dependencies = append(dependencies, word.String())
			word.Reset()
		}
	}
	for index := colon + 1; index < len(data); index++ {
		b := data[index]
		if b == '\\' {
			if index+1 >= len(data) {
				return nil, errors.New("dependency file ends in an escape")
			}
			next := data[index+1]
			if next == '\n' {
				index++
				continue
			}
			if next == '\r' && index+2 < len(data) && data[index+2] == '\n' {
				index += 2
				continue
			}
			word.WriteByte(next)
			index++
			continue
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			flush()
			continue
		}
		word.WriteByte(b)
	}
	flush()
	if len(dependencies) == 0 {
		return nil, errors.New("dependency file has no dependencies")
	}
	return dependencies, nil
}

func pathAliases(path string) []string {
	clean := filepath.Clean(path)
	aliases := []string{clean, filepath.ToSlash(clean)}
	if absolute, err := filepath.Abs(clean); err == nil {
		aliases = append(aliases, absolute, filepath.ToSlash(absolute))
	}
	return aliases
}

func canonicalDependencies(cfg config, dependencies []string) ([]string, error) {
	mappings := make(map[string]string, len(cfg.physical)*4)
	for index, physical := range cfg.physical {
		canonical := filepath.ToSlash(filepath.Clean(cfg.canonical[index]))
		if canonical == "." || filepath.IsAbs(canonical) {
			return nil, fmt.Errorf("invalid canonical source path %q", cfg.canonical[index])
		}
		for _, alias := range pathAliases(physical) {
			if previous, ok := mappings[alias]; ok && previous != canonical {
				return nil, fmt.Errorf("physical path %q maps to both %q and %q", physical, previous, canonical)
			}
			mappings[alias] = canonical
		}
	}

	seen := map[string]bool{filepath.ToSlash(filepath.Clean(cfg.primary)): true}
	var result []string
	for _, dependency := range dependencies {
		canonical := ""
		for _, alias := range pathAliases(dependency) {
			if value, ok := mappings[alias]; ok {
				canonical = value
				break
			}
		}
		if canonical == "" || seen[canonical] {
			continue
		}
		seen[canonical] = true
		result = append(result, canonical)
	}
	return result, nil
}

func run(cfg config) error {
	data, err := os.ReadFile(cfg.depfile)
	if err != nil {
		return fmt.Errorf("read depfile: %w", err)
	}
	dependencies, err := depfileDependencies(data)
	if err != nil {
		return err
	}
	canonical, err := canonicalDependencies(cfg, dependencies)
	if err != nil {
		return err
	}

	var output strings.Builder
	fmt.Fprintf(&output, "source_%s := %s\n\n", cfg.object, filepath.ToSlash(filepath.Clean(cfg.primary)))
	fmt.Fprintf(&output, "deps_%s := \\\n", cfg.object)
	for _, path := range canonical {
		fmt.Fprintf(&output, "  %s \\\n", path)
	}
	fmt.Fprintf(&output, "\n%s: $(deps_%s)\n\n$(deps_%s):\n", cfg.object, cfg.object, cfg.object)
	if cfg.symversions != "" {
		symversions, err := os.ReadFile(cfg.symversions)
		if err != nil {
			return fmt.Errorf("read symbol versions: %w", err)
		}
		output.Write(symversions)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(cfg.out, []byte(output.String()), 0o600)
}

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "sourceversioncmd: %v\n", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "sourceversioncmd: %v\n", err)
		os.Exit(1)
	}
}
