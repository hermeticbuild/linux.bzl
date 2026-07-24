package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: runandwrite OUTPUT TOOL [ARG...]")
		os.Exit(2)
	}
	if err := runAndWrite(os.Args[1], os.Args[2], os.Args[3:]); err != nil {
		fmt.Fprintf(os.Stderr, "runandwrite: %v\n", err)
		os.Exit(1)
	}
}

func runAndWrite(outputPath, toolPath string, args []string) error {
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	cmd := exec.Command(toolPath, args...)
	cmd.Env = []string{}
	cmd.Stdout = output
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	closeErr := output.Close()
	if runErr != nil {
		return runErr
	}
	return closeErr
}
