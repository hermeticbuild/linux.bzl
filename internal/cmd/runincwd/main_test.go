package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cfg, err := parseArgs([]string{
		"-cwd", "work",
		"-env", "FIRST=value=with=equals",
		"-env=SECOND=",
		"--",
		"tools/modpost",
		"-M",
	})
	if err != nil {
		t.Fatalf("parseArgs() error: %v", err)
	}
	if cfg.cwd != "work" {
		t.Errorf("cwd = %q, want work", cfg.cwd)
	}
	if got, want := strings.Join(cfg.env, "\n"), "FIRST=value=with=equals\nSECOND="; got != want {
		t.Errorf("env = %q, want %q", got, want)
	}
	if cfg.tool != "tools/modpost" {
		t.Errorf("tool = %q, want tools/modpost", cfg.tool)
	}
	if got, want := strings.Join(cfg.toolArgs, "\n"), "-M"; got != want {
		t.Errorf("toolArgs = %q, want %q", got, want)
	}
}

func TestParseArgsRejectsInvalidInvocation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing separator", args: []string{"-cwd", "work", "tool"}, want: "missing --"},
		{name: "missing tool", args: []string{"-cwd", "work", "--"}, want: "missing TOOL"},
		{name: "missing cwd", args: []string{"--", "tool"}, want: "-cwd is required"},
		{name: "unexpected positional", args: []string{"-cwd", "work", "extra", "--", "tool"}, want: "unexpected argument"},
		{name: "unknown flag", args: []string{"-cwd", "work", "-bogus", "x", "--", "tool"}, want: "flag provided but not defined"},
		{name: "env without value", args: []string{"-cwd", "work", "-env", "NAME", "--", "tool"}, want: "NAME=VALUE"},
		{name: "invalid env name", args: []string{"-cwd", "work", "-env", "BAD-NAME=value", "--", "tool"}, want: "invalid environment variable name"},
		{name: "empty env name", args: []string{"-cwd", "work", "-env", "=value", "--", "tool"}, want: "invalid environment variable name"},
		{name: "duplicate env", args: []string{"-cwd", "work", "-env", "NAME=one", "-env", "NAME=two", "--", "tool"}, want: "duplicate environment variable"},
		{name: "nul in env", args: []string{"-cwd", "work", "-env", "NAME=one\x00two", "--", "tool"}, want: "NUL byte"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseArgs(test.args)
			if err == nil {
				t.Fatal("parseArgs() succeeded, want error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseArgs() error = %q, want substring %q", err, test.want)
			}
		})
	}
}

func TestRunUsesWorkingDirectoryAndExplicitEnvironment(t *testing.T) {
	t.Setenv("RUNINCWD_SHOULD_NOT_BE_INHERITED", "parent-value")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err = run(config{
		cwd:      workdir,
		env:      []string{"RUNINCWD_VISIBLE=yes", "RUNINCWD_CWD={cwd}/child"},
		tool:     executable,
		toolArgs: []string{"-test.run=TestRunincwdHelperProcess", "--", "inspect"},
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error: %v\nstderr:\n%s", err, stderr.String())
	}

	wantDir, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("stdout lines = %q, want cwd, explicit env, expanded cwd, and inherited env", lines)
	}
	gotDir, err := filepath.EvalSymlinks(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	if gotDir != wantDir {
		t.Errorf("child cwd = %q, want %q", gotDir, wantDir)
	}
	if lines[1] != "yes" {
		t.Errorf("explicit environment = %q, want yes", lines[1])
	}
	wantExpanded := filepath.Join(wantDir, "child")
	gotExpanded, err := filepath.EvalSymlinks(filepath.Dir(lines[2]))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Join(gotExpanded, filepath.Base(lines[2])) != wantExpanded {
		t.Errorf("expanded cwd environment = %q, want %q", lines[2], wantExpanded)
	}
	if lines[3] != "<empty>" {
		t.Errorf("inherited environment = %q, want <empty>", lines[3])
	}
	if got := strings.TrimSpace(stderr.String()); got != "child stderr" {
		t.Errorf("stderr = %q, want %q", got, "child stderr")
	}
}

func TestRunMainPropagatesChildExitCode(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runMain([]string{
		"-cwd", t.TempDir(),
		"--", executable,
		"-test.run=TestRunincwdHelperProcess", "--", "exit",
	}, &stdout, &stderr)
	if code != 17 {
		t.Fatalf("runMain() exit code = %d, want 17\nstderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "child failure") {
		t.Errorf("stderr = %q, want child failure", stderr.String())
	}
}

func TestRunRejectsNonDirectoryWorkingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(config{cwd: path, tool: "unused"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("run() error = %v, want non-directory error", err)
	}
}

func TestRunincwdHelperProcess(t *testing.T) {
	args := os.Args
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(args) {
		return
	}

	switch args[separator+1] {
	case "inspect":
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		inherited := os.Getenv("RUNINCWD_SHOULD_NOT_BE_INHERITED")
		if inherited == "" {
			inherited = "<empty>"
		}
		fmt.Printf("%s\n%s\n%s\n%s\n", cwd, os.Getenv("RUNINCWD_VISIBLE"), os.Getenv("RUNINCWD_CWD"), inherited)
		fmt.Fprintln(os.Stderr, "child stderr")
		os.Exit(0)
	case "exit":
		fmt.Fprintln(os.Stderr, "child failure")
		os.Exit(17)
	}
}
