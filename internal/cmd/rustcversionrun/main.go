package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	expected := flag.String("expected", "", "required rustc release")
	flag.Parse()
	args := flag.Args()
	if *expected == "" {
		fatalf("-expected is required")
	}
	if len(args) == 0 {
		fatalf("rustc command is required")
	}

	version := exec.Command(args[0], "--version")
	version.Env = os.Environ()
	var output bytes.Buffer
	version.Stdout = &output
	version.Stderr = &output
	if err := version.Run(); err != nil {
		fatalf("%s --version failed: %v\n%s", args[0], err, output.String())
	}
	release, err := rustcRelease(output.String())
	if err != nil || release != *expected {
		fatalf("expected rustc %s, got %q", *expected, strings.TrimSpace(output.String()))
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fatalf("running rustc: %v", err)
	}
}

func rustcRelease(output string) (string, error) {
	fields := strings.Fields(output)
	if len(fields) < 2 || fields[0] != "rustc" {
		return "", fmt.Errorf("invalid rustc version output %q", strings.TrimSpace(output))
	}
	return fields[1], nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "rustcversionrun: "+format+"\n", args...)
	os.Exit(1)
}
