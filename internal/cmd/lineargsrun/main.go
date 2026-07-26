package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

const usage = "usage: lineargsrun -args_file FILE -output FILE -- TOOL [ARG...]"

type config struct {
	argsFile string
	output   string
	tool     string
	toolArgs []string
}

func parseArgs(args []string) (config, error) {
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return config{}, errors.New("missing -- separator before TOOL")
	}
	if separator+1 >= len(args) {
		return config{}, errors.New("missing TOOL after -- separator")
	}

	var cfg config
	flags := flag.NewFlagSet("lineargsrun", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.argsFile, "args_file", "", "line-oriented arguments file")
	flags.StringVar(&cfg.output, "output", "", "output that TOOL must create")
	if err := flags.Parse(args[:separator]); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected argument before -- separator: %q", flags.Arg(0))
	}
	if cfg.argsFile == "" || cfg.output == "" {
		return config{}, errors.New("-args_file and -output are required")
	}
	cfg.tool = args[separator+1]
	cfg.toolArgs = append([]string(nil), args[separator+2:]...)
	return cfg, nil
}

func readLineArgs(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var args []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		args = append(args, strings.Fields(line)...)
	}
	return args, scanner.Err()
}

func expandedArgs(template, fileArgs []string, output string) ([]string, error) {
	var out []string
	foundFile := false
	for _, arg := range template {
		switch arg {
		case "{args_file}":
			foundFile = true
			out = append(out, fileArgs...)
		case "{output}":
			out = append(out, output)
		default:
			out = append(out, arg)
		}
	}
	if !foundFile {
		return nil, errors.New("TOOL arguments are missing {args_file}")
	}
	return out, nil
}

func run(cfg config, stdout, stderr io.Writer) error {
	fileArgs, err := readLineArgs(cfg.argsFile)
	if err != nil {
		return err
	}
	args, err := expandedArgs(cfg.toolArgs, fileArgs, cfg.output)
	if err != nil {
		return err
	}
	cmd := exec.Command(cfg.tool, args...)
	cmd.Env = []string{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	if _, err := os.Stat(cfg.output); err != nil {
		return fmt.Errorf("tool did not create output %q: %w", cfg.output, err)
	}
	return nil
}

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "lineargsrun: %v\n%s\n", err, usage)
		os.Exit(2)
	}
	if err := run(cfg, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "lineargsrun: %v\n", err)
		os.Exit(1)
	}
}
