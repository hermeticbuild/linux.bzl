package kconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/ccprofile"
)

func TestGraphProfileShellResolvesAndProjectsExactCommand(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "scripts", "probe.sh")
	helper := filepath.Join(root, "scripts", "helper.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("#!/bin/sh\n$(dirname $0)/helper.sh\n")
	helperContent := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(script, content, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, helperContent, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	helperSum := sha256.Sum256(helperContent)
	environment := map[string]string{"CC": "clang", "SRCARCH": "x86"}
	canonical := graphProfileSourceRoot + "/scripts/probe.sh clang"
	identity, err := ccprofile.NewKconfigCommandIdentity(
		ccprofile.KconfigCommandKindSuccess,
		canonical,
		environment,
		map[string]string{
			"scripts/helper.sh": hex.EncodeToString(helperSum[:]),
			"scripts/probe.sh":  hex.EncodeToString(sum[:]),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	success := true
	profile := ccprofile.GraphProfile{
		Schema:         ccprofile.GraphProfileSchema,
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigCommands: []ccprofile.KconfigCommand{{
			ID:          identity.ID,
			Kind:        identity.Kind,
			Command:     identity.Command,
			Environment: identity.Environment,
			Inputs:      identity.Inputs,
			Success:     &success,
		}},
	}
	shell, err := NewGraphProfileShell(profile, root, environment)
	if err != nil {
		t.Fatal(err)
	}
	command := `{ ` + filepath.ToSlash(script) + ` clang; } >/dev/null 2>&1 && echo "y" || echo "n"`
	got, err := shell.Run(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if got != "y" {
		t.Fatalf("Run() = %q, want y", got)
	}
	projection, err := shell.Projection()
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.KconfigCommands) != 1 ||
		projection.KconfigCommands[0].ID != identity.ID {
		t.Fatalf("Projection() commands = %#v", projection.KconfigCommands)
	}

	if err := os.WriteFile(script, append(content, []byte("# changed\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = shell.Run(context.Background(), command)
	if err == nil || !strings.Contains(err.Error(), "missing from graph profile") {
		t.Fatalf("Run() after input change error = %v", err)
	}
}

func TestGraphProfileShellResolvesRelativeSourceRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "source")
	script := filepath.Join(root, "scripts", "probe.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("#!/bin/sh\necho ok\n")
	if err := os.WriteFile(script, content, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	environment := map[string]string{"CC": "clang"}
	identity, err := ccprofile.NewKconfigCommandIdentity(
		ccprofile.KconfigCommandKindStdout,
		graphProfileSourceRoot+"/scripts/probe.sh",
		environment,
		map[string]string{
			"scripts/probe.sh": hex.EncodeToString(sum[:]),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	stdout := "ok"
	profile := ccprofile.GraphProfile{
		Schema:         ccprofile.GraphProfileSchema,
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigCommands: []ccprofile.KconfigCommand{{
			ID:          identity.ID,
			Kind:        identity.Kind,
			Command:     identity.Command,
			Environment: identity.Environment,
			Inputs:      identity.Inputs,
			Stdout:      &stdout,
		}},
	}
	t.Chdir(parent)
	shell, err := NewGraphProfileShell(profile, "source", environment)
	if err != nil {
		t.Fatal(err)
	}
	got, err := shell.Run(context.Background(), "source/scripts/probe.sh")
	if err != nil {
		t.Fatal(err)
	}
	if got != stdout {
		t.Fatalf("Run() = %q, want %q", got, stdout)
	}
}

func TestGraphProfileShellTracksTransitiveSiblingInputChange(t *testing.T) {
	root := t.TempDir()
	scripts := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(scripts, "probe.sh")
	helper := filepath.Join(scripts, "helper.sh")
	scriptContent := []byte("#!/bin/sh\n$(dirname \"$0\")/helper.sh\n")
	helperContent := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(script, scriptContent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, helperContent, 0o755); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"CC": "clang"}
	command := filepath.ToSlash(script)
	recording, err := NewGraphProfileRecordingShell(
		ccprofile.GraphProfileIdentity{
			Architecture:   "x86_64",
			DriverContract: ccprofile.DriverContract,
			AnalysisIdentity: ccprofile.AnalysisIdentity{
				Compiler:            "clang",
				TargetGNUSystemName: "x86_64-unknown-linux-gnu",
			},
		},
		root,
		environment,
		func(context.Context, string) (string, error) { return "ok", nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recording.Run(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	profile, err := recording.RecordedProfile()
	if err != nil {
		t.Fatal(err)
	}
	resolving, err := NewGraphProfileShell(profile, root, environment)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, append(helperContent, []byte("# changed\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = resolving.Run(context.Background(), command)
	if err == nil || !strings.Contains(err.Error(), "missing from graph profile") {
		t.Fatalf("Run() after helper change error = %v", err)
	}
}

func TestCanonicalGraphProfileCommandPreservesUnrelatedBackslashes(t *testing.T) {
	const sourceRoot = `C:\src`
	for _, test := range []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "native source root",
			command: `printf '\n' C:\src\scripts\probe.sh`,
			want:    `printf '\n' ` + graphProfileSourceRoot + `/scripts/probe.sh`,
		},
		{
			name:    "slash source root",
			command: `printf '\n' C:/src/scripts/probe.sh`,
			want:    `printf '\n' ` + graphProfileSourceRoot + `/scripts/probe.sh`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := canonicalGraphProfileCommand(test.command, sourceRoot); got != test.want {
				t.Fatalf("canonicalGraphProfileCommand() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestGraphProfileRecordingShellCapturesTypedResults(t *testing.T) {
	root := t.TempDir()
	identity := ccprofile.GraphProfileIdentity{
		Architecture:   "aarch64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "aarch64-unknown-linux-gnu",
		},
	}
	fallback := func(_ context.Context, command string) (string, error) {
		if strings.Contains(command, "&& echo") {
			return "n", nil
		}
		return "Clang 220108", nil
	}
	shell, err := NewGraphProfileRecordingShell(
		identity,
		root,
		map[string]string{"CC": "clang"},
		fallback,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := shell.Run(context.Background(), "clang --version"); err != nil || got != "Clang 220108" {
		t.Fatalf("stdout Run() = %q, %v", got, err)
	}
	if got, err := shell.Run(
		context.Background(),
		`{ test "Clang" = GCC; } >/dev/null 2>&1 && echo "y" || echo "n"`,
	); err != nil || got != "n" {
		t.Fatalf("success Run() = %q, %v", got, err)
	}
	profile, err := shell.RecordedProfile()
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.KconfigCommands) != 2 {
		t.Fatalf("RecordedProfile() has %d commands, want 2", len(profile.KconfigCommands))
	}
	kinds := map[string]bool{}
	for _, command := range profile.KconfigCommands {
		kinds[command.Kind] = true
	}
	if !kinds[ccprofile.KconfigCommandKindStdout] ||
		!kinds[ccprofile.KconfigCommandKindSuccess] {
		t.Fatalf("recorded command kinds = %#v", kinds)
	}
}

func TestKbuildGraphProbeInputsHashExplicitFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string][]byte{
		"include/probe.h":      []byte("#define PROBE 1\n"),
		"include/macros.h":     []byte("#define MACRO 1\n"),
		"scripts/probe.lds":    []byte("SECTIONS {}\n"),
		"scripts/versions.map": []byte("VMLINUX_1 { global: probe; };\n"),
		"probe.rsp":            []byte("-DRESPONSE=1\n"),
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shell, err := NewGraphProfileRecordingShell(
		ccprofile.GraphProfileIdentity{
			Architecture:   "x86_64",
			DriverContract: ccprofile.DriverContract,
			AnalysisIdentity: ccprofile.AnalysisIdentity{
				Compiler:            "clang",
				TargetGNUSystemName: "x86_64-unknown-linux-gnu",
			},
		},
		root,
		nil,
		func(context.Context, string) (string, error) { return "", nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	contextArgv := []string{
		"-include", graphProfileSourceRoot + "/include/probe.h",
		"-imacros" + graphProfileSourceRoot + "/include/macros.h",
		"-Wl,-T," + graphProfileSourceRoot + "/scripts/probe.lds",
		"-Wl,--version-script=" + graphProfileSourceRoot + "/scripts/versions.map",
	}
	candidateArgv := []string{"@" + graphProfileSourceRoot + "/probe.rsp"}
	inputs, err := shell.kbuildGraphProbeInputs(contextArgv, candidateArgv)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(inputs), len(files); got != want {
		t.Fatalf("inputs = %#v, want %d entries", inputs, want)
	}
	for name, content := range files {
		sum := sha256.Sum256(content)
		if got, want := inputs[name], hex.EncodeToString(sum[:]); got != want {
			t.Errorf("input %s digest = %q, want %q", name, got, want)
		}
	}
	first, err := ccprofile.NewKbuildGraphProbeIdentity(
		ccprofile.KbuildGraphProbeKindCCOption,
		"c",
		contextArgv,
		candidateArgv,
		inputs,
	)
	if err != nil {
		t.Fatal(err)
	}
	changed := []byte("#define PROBE 2\n")
	if err := os.WriteFile(filepath.Join(root, "include", "probe.h"), changed, 0o644); err != nil {
		t.Fatal(err)
	}
	inputs, err = shell.kbuildGraphProbeInputs(contextArgv, candidateArgv)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ccprofile.NewKbuildGraphProbeIdentity(
		ccprofile.KbuildGraphProbeKindCCOption,
		"c",
		contextArgv,
		candidateArgv,
		inputs,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("probe identity did not change after an explicit input changed")
	}
}

func TestKbuildGraphProbeInputsRejectUnrepresentablePaths(t *testing.T) {
	root := t.TempDir()
	shell, err := NewGraphProfileRecordingShell(
		ccprofile.GraphProfileIdentity{
			Architecture:   "x86_64",
			DriverContract: ccprofile.DriverContract,
		},
		root,
		nil,
		func(context.Context, string) (string, error) { return "", nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "probe.h")
	if err := os.WriteFile(outside, []byte("#define OUTSIDE 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		argv []string
		want string
	}{
		{
			name: "object tree",
			argv: []string{"-include", KbuildGraphProbeObjectRoot + "/generated/probe.h"},
			want: "generated in the object tree",
		},
		{
			name: "outside source root",
			argv: []string{"-include", filepath.ToSlash(outside)},
			want: "outside source root",
		},
		{
			name: "missing option operand",
			argv: []string{"-imacros"},
			want: "has no input path",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := shell.kbuildGraphProbeInputs(test.argv, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("kbuildGraphProbeInputs() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestGraphProfileExtensionUsesRefreshedDecisionAndRecordsSuperset(t *testing.T) {
	root := t.TempDir()
	environment := map[string]string{"CC": "clang", "LD": "ld.lld"}
	decision := "test refreshed-compiler-branch"
	openedBranchCommand := `{ test -e plugin/include/plugin-version.h; } >/dev/null 2>&1 && echo "y" || echo "n"`
	decisionIdentity, err := ccprofile.NewKconfigCommandIdentity(
		ccprofile.KconfigCommandKindSuccess,
		decision,
		environment,
		map[string]string{},
	)
	if err != nil {
		t.Fatal(err)
	}
	success := true
	seed := ccprofile.GraphProfile{
		Schema:         ccprofile.GraphProfileSchema,
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigCommands: []ccprofile.KconfigCommand{{
			ID:          decisionIdentity.ID,
			Kind:        decisionIdentity.Kind,
			Command:     decisionIdentity.Command,
			Environment: decisionIdentity.Environment,
			Inputs:      decisionIdentity.Inputs,
			Success:     &success,
		}},
		KbuildGraphProbes: []ccprofile.KbuildGraphProbe{},
	}
	commandCalls := 0
	probeCalls := 0
	shell, err := NewGraphProfileExtensionShell(
		seed,
		root,
		environment,
		func(_ context.Context, command string) (string, error) {
			commandCalls++
			if command != openedBranchCommand {
				return "", fmt.Errorf("unexpected command %q", command)
			}
			return "y", nil
		},
		func(request ccprofile.KbuildGraphProbeIdentity) (bool, error) {
			probeCalls++
			if request.Kind != ccprofile.KbuildGraphProbeKindCCOption {
				return false, fmt.Errorf("unexpected probe kind %q", request.Kind)
			}
			return true, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	decisionOutput, err := shell.Run(
		context.Background(),
		`{ `+decision+`; } >/dev/null 2>&1 && echo "y" || echo "n"`,
	)
	if err != nil {
		t.Fatalf("resolve refreshed decision: %v", err)
	}
	if decisionOutput != "y" {
		t.Fatalf("refreshed decision = %q, want y", decisionOutput)
	}
	if commandCalls != 0 {
		t.Fatalf("exact seed hit executed fallback %d times", commandCalls)
	}

	// The refreshed result opens a branch that deterministic recording never
	// visited, so its command is absent from the seed and must be extended.
	if decisionOutput == "y" {
		if got, err := shell.Run(context.Background(), openedBranchCommand); err != nil || got != "y" {
			t.Fatalf("new branch Run() = %q, %v", got, err)
		}
		if got, err := shell.Run(context.Background(), openedBranchCommand); err != nil || got != "y" {
			t.Fatalf("repeated new branch Run() = %q, %v", got, err)
		}
	}
	if commandCalls != 1 {
		t.Fatalf("new command fallback calls = %d, want 1", commandCalls)
	}

	for attempt := 0; attempt < 2; attempt++ {
		supported, err := shell.resolveKbuildGraphProbe(
			ccprofile.KbuildGraphProbeKindCCOption,
			"c",
			[]string{"-Werror"},
			[]string{"-fnew-branch"},
			map[string]string{},
			false,
		)
		if err != nil {
			t.Fatalf("resolve missing Kbuild probe: %v", err)
		}
		if !supported {
			t.Fatal("extended Kbuild probe is unsupported, want supported")
		}
	}
	if probeCalls != 1 {
		t.Fatalf("new probe evaluator calls = %d, want 1", probeCalls)
	}

	extended, err := shell.RecordedProfile()
	if err != nil {
		t.Fatalf("RecordedProfile() failed: %v", err)
	}
	if err := ccprofile.ValidateGraphProfile(extended); err != nil {
		t.Fatalf("extended profile is invalid: %v", err)
	}
	if got, want := len(extended.KconfigCommands), 2; got != want {
		t.Fatalf("extended command count = %d, want %d", got, want)
	}
	if got, want := len(extended.KbuildGraphProbes), 1; got != want {
		t.Fatalf("extended probe count = %d, want %d", got, want)
	}
	if _, err := ccprofile.ResolveKconfigCommand(extended, decisionIdentity); err != nil {
		t.Fatalf("extended profile lost seed command: %v", err)
	}
	projection, err := shell.Projection()
	if err != nil {
		t.Fatalf("Projection() failed: %v", err)
	}
	if got, want := len(projection.KconfigCommands), 2; got != want {
		t.Fatalf("projection command count = %d, want %d", got, want)
	}
	if got, want := len(projection.KbuildGraphProbes), 1; got != want {
		t.Fatalf("projection probe count = %d, want %d", got, want)
	}
}
