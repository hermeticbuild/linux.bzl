package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/hermeticbuild/linux.bzl/internal/rusttoolchain"
)

type versionPredicate struct {
	AtLeast    string   `json:"at_least"`
	Add        []string `json:"add"`
	Remove     []string `json:"remove"`
	ElseAdd    []string `json:"else_add"`
	ElseRemove []string `json:"else_remove"`
}

type predicateFlags []versionPredicate

func (f *predicateFlags) String() string {
	return fmt.Sprintf("%d predicates", len(*f))
}

func (f *predicateFlags) Set(value string) error {
	var predicate versionPredicate
	if err := json.Unmarshal([]byte(value), &predicate); err != nil {
		return err
	}
	if predicate.AtLeast == "" {
		return errors.New("version predicate requires at_least")
	}
	*f = append(*f, predicate)
	return nil
}

func parseArgs(args []string) (string, []versionPredicate, []string, error) {
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(args) {
		return "", nil, nil, errors.New("expected -- RUSTC [ARG...]")
	}
	var predicates predicateFlags
	flags := flag.NewFlagSet("rustcrun", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	probe := flags.String("probe", "", "Rust toolchain probe JSON")
	flags.Var(&predicates, "predicate", "JSON version predicate")
	if err := flags.Parse(args[:separator]); err != nil {
		return "", nil, nil, err
	}
	if *probe == "" {
		return "", nil, nil, errors.New("-probe is required")
	}
	return *probe, predicates, args[separator+1:], nil
}

func applyPredicates(probe rusttoolchain.Probe, args []string, predicates []versionPredicate) ([]string, error) {
	out := append([]string(nil), args...)
	for _, predicate := range predicates {
		matches, err := probe.AtLeast(predicate.AtLeast)
		if err != nil {
			return nil, fmt.Errorf("invalid at_least %q: %w", predicate.AtLeast, err)
		}
		add, remove := predicate.ElseAdd, predicate.ElseRemove
		if matches {
			add, remove = predicate.Add, predicate.Remove
		}
		for _, removed := range remove {
			filtered := out[:0]
			for _, arg := range out {
				if arg != removed {
					filtered = append(filtered, arg)
				}
			}
			out = filtered
		}
		out = append(out, add...)
	}
	return out, nil
}

func run(args []string) error {
	probePath, predicates, command, err := parseArgs(args)
	if err != nil {
		return err
	}
	probeFile, err := os.Open(probePath)
	if err != nil {
		return err
	}
	probe, decodeErr := rusttoolchain.Decode(probeFile)
	closeErr := probeFile.Close()
	if decodeErr != nil {
		return decodeErr
	}
	if closeErr != nil {
		return closeErr
	}
	command, err = applyPredicates(probe, command, predicates)
	if err != nil {
		return err
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "rustcrun: %v\n", err)
		os.Exit(1)
	}
}
