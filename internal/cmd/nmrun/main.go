// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	nm := flag.String("nm", "", "llvm-nm executable")
	in := flag.String("in", "", "input binary")
	out := flag.String("out", "", "output llvm-nm data")
	flag.Parse()

	if *nm == "" || *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-nm, -in, and -out are required")
		os.Exit(2)
	}
	if err := run(*nm, *in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "nmrun: %v\n", err)
		os.Exit(1)
	}
}

func run(nmPath, inPath, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	cmd := exec.Command(nmPath, "-n", inPath)
	cmd.Env = []string{}
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	closeErr := out.Close()
	if runErr != nil {
		return runErr
	}
	return closeErr
}
