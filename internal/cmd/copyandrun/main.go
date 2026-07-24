package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: copyandrun INPUT OUTPUT TOOL [ARG...]")
		os.Exit(2)
	}
	if err := copyAndRun(os.Args[1], os.Args[2], os.Args[3], os.Args[4:]); err != nil {
		fmt.Fprintf(os.Stderr, "copyandrun: %v\n", err)
		os.Exit(1)
	}
}

func copyAndRun(inputPath, outputPath, toolPath string, args []string) error {
	input, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer input.Close()

	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}

	toolArgs := append([]string{outputPath}, args...)
	cmd := exec.Command(toolPath, toolArgs...)
	cmd.Env = []string{}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
