package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/rusttoolchain"
)

type envFlag []string

func (f *envFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *envFlag) Set(value string) error {
	name, _, ok := strings.Cut(value, "=")
	if !ok || name == "" {
		return fmt.Errorf("expected NAME=VALUE")
	}
	*f = append(*f, value)
	return nil
}

func inspectRustc(path string, env []string) (rusttoolchain.Probe, error) {
	cmd := exec.Command(path, "-vV")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return rusttoolchain.Probe{}, fmt.Errorf("rustc -vV failed: %w\n%s", err, stderr.String())
	}
	probe, err := rusttoolchain.ParseVerbose(stdout.String())
	if err != nil {
		return rusttoolchain.Probe{}, err
	}
	return probe, nil
}

func validateCompilerIdentity(target, host rusttoolchain.Probe) error {
	if target == host {
		return nil
	}
	return fmt.Errorf(
		"target rustc identity (%s, commit %s, LLVM %s) does not match host rustc identity (%s, commit %s, LLVM %s)",
		target.VersionText,
		target.CommitHash,
		target.LLVMVersion,
		host.VersionText,
		host.CommitHash,
		host.LLVMVersion,
	)
}

func main() {
	var env, hostEnv envFlag
	rustc := flag.String("rustc", "", "Path to rustc")
	hostRustc := flag.String("host-rustc", "", "Path to host rustc")
	out := flag.String("out", "", "Output JSON path")
	minimum := flag.String("minimum", "", "Minimum supported rustc release")
	flag.Var(&env, "env", "Hermetic rustc environment entry")
	flag.Var(&hostEnv, "host-env", "Hermetic host rustc environment entry")
	flag.Parse()
	if *rustc == "" || *hostRustc == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "rusttoolchainprobe: -rustc, -host-rustc, and -out are required")
		os.Exit(2)
	}

	probe, err := inspectRustc(*rustc, []string(env))
	if err != nil {
		fmt.Fprintf(os.Stderr, "rusttoolchainprobe: target %v\n", err)
		os.Exit(1)
	}
	hostProbe, err := inspectRustc(*hostRustc, []string(hostEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "rusttoolchainprobe: host %v\n", err)
		os.Exit(1)
	}
	if err := validateCompilerIdentity(probe, hostProbe); err != nil {
		fmt.Fprintf(os.Stderr, "rusttoolchainprobe: %v\n", err)
		os.Exit(1)
	}
	if *minimum != "" {
		ok, err := probe.AtLeast(*minimum)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rusttoolchainprobe: invalid minimum %q: %v\n", *minimum, err)
			os.Exit(2)
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "rusttoolchainprobe: kernel requires rustc >= %s, selected %s\n", *minimum, probe.Release)
			os.Exit(1)
		}
	}
	file, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rusttoolchainprobe: create output: %v\n", err)
		os.Exit(1)
	}
	if err := rusttoolchain.Encode(file, probe); err != nil {
		file.Close()
		fmt.Fprintf(os.Stderr, "rusttoolchainprobe: encode output: %v\n", err)
		os.Exit(1)
	}
	if err := file.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "rusttoolchainprobe: close output: %v\n", err)
		os.Exit(1)
	}
}
