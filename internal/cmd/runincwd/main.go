package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const usage = "usage: runincwd -cwd DIR [-env NAME=VALUE ...] -- TOOL [ARG...]"

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type config struct {
	cwd      string
	env      []string
	tool     string
	toolArgs []string
}

type envValues struct {
	entries []string
	names   map[string]struct{}
}

func (v *envValues) String() string {
	return strings.Join(v.entries, ",")
}

func (v *envValues) Set(entry string) error {
	name, _, ok := strings.Cut(entry, "=")
	if !ok {
		return fmt.Errorf("environment entry %q must have the form NAME=VALUE", entry)
	}
	if !envNamePattern.MatchString(name) {
		return fmt.Errorf("invalid environment variable name %q", name)
	}
	if strings.IndexByte(entry, 0) >= 0 {
		return fmt.Errorf("environment entry for %q contains a NUL byte", name)
	}
	if v.names == nil {
		v.names = make(map[string]struct{})
	}
	if _, ok := v.names[name]; ok {
		return fmt.Errorf("duplicate environment variable %q", name)
	}
	v.names[name] = struct{}{}
	v.entries = append(v.entries, entry)
	return nil
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

	var cwd string
	var env envValues
	flags := flag.NewFlagSet("runincwd", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cwd, "cwd", "", "working directory for TOOL")
	flags.Var(&env, "env", "environment entry passed to TOOL (repeatable)")
	if err := flags.Parse(args[:separator]); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected argument before -- separator: %q", flags.Arg(0))
	}
	if cwd == "" {
		return config{}, errors.New("-cwd is required")
	}

	return config{
		cwd:      cwd,
		env:      append([]string(nil), env.entries...),
		tool:     args[separator+1],
		toolArgs: append([]string(nil), args[separator+2:]...),
	}, nil
}

func run(cfg config, stdout, stderr io.Writer) error {
	cwd, err := filepath.Abs(cfg.cwd)
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return fmt.Errorf("working directory %q: %w", cfg.cwd, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working directory %q is not a directory", cfg.cwd)
	}

	tool := cfg.tool
	if !filepath.IsAbs(tool) {
		tool, err = filepath.Abs(tool)
		if err != nil {
			return fmt.Errorf("resolve tool %q: %w", cfg.tool, err)
		}
	}

	cmd := exec.Command(tool, cfg.toolArgs...)
	cmd.Dir = cwd
	cmd.Env = make([]string, len(cfg.env))
	for i, entry := range cfg.env {
		cmd.Env[i] = strings.ReplaceAll(entry, "{cwd}", cwd)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 1
	}
	if code := exitErr.ExitCode(); code >= 0 {
		return code
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return 1
}

func runMain(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "runincwd: %v\n%s\n", err, usage)
		return 2
	}
	if err := run(cfg, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "runincwd: %v\n", err)
		return exitCode(err)
	}
	return 0
}

func main() {
	os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr))
}
