package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

func main() {
	args := os.Args[1:]
	var stdinPath string
	if len(args) >= 2 && args[0] == "-stdin" {
		stdinPath = args[1]
		args = args[2:]
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: runandwrite [-stdin INPUT] OUTPUT TOOL [ARG...]")
		os.Exit(2)
	}
	if err := runAndWrite(stdinPath, args[0], args[1], args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "runandwrite: %v\n", err)
		os.Exit(1)
	}
}

func runAndWrite(stdinPath, outputPath, toolPath string, args []string) error {
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	cmd := exec.Command(toolPath, args...)
	cmd.Env = []string{}
	var input io.ReadCloser
	if stdinPath != "" {
		input, err = os.Open(stdinPath)
		if err != nil {
			output.Close()
			return err
		}
		defer input.Close()
		cmd.Stdin = input
	}
	cmd.Stdout = output
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	closeErr := output.Close()
	if runErr != nil {
		return runErr
	}
	return closeErr
}
