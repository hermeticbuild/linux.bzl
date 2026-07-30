package ccprofile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplayConfiguredKconfigCommandsUsesSelectedTools(t *testing.T) {
	root := t.TempDir()
	compiler := filepath.Join(root, "selected-compiler")
	linker := filepath.Join(root, "selected-linker")
	writeProbeTool(t, compiler, `#!/bin/sh
case "$1" in
  --supported) exit 0 ;;
  --identity) printf 'configured\ncompiler\n'; exit 0 ;;
esac
exit 1
`)
	writeProbeTool(t, linker, "#!/bin/sh\nexit 0\n")

	success := true
	stdout := "configured compiler"
	commands := []KconfigCommand{
		configuredKconfigCommand(t, KconfigCommand{
			Kind:        KconfigCommandKindSuccess,
			Command:     "clang --supported",
			Environment: map[string]string{"CC": "clang", "LD": "ld.lld"},
			Inputs:      map[string]string{},
			Success:     &success,
		}),
		configuredKconfigCommand(t, KconfigCommand{
			Kind:        KconfigCommandKindStdout,
			Command:     "$CC --identity",
			Environment: map[string]string{"CC": "clang", "LD": "ld.lld"},
			Inputs:      map[string]string{},
			Stdout:      &stdout,
		}),
	}
	got, err := ReplayConfiguredKconfigCommands(
		context.Background(),
		commands,
		KbuildGraphProbeTools{
			Compiler:   compiler,
			Linker:     linker,
			Shell:      "/bin/sh",
			SourceRoot: root,
		},
	)
	if err != nil {
		t.Fatalf("ReplayConfiguredKconfigCommands() failed: %v", err)
	}
	if got != 2 {
		t.Fatalf("replayed commands = %d, want 2", got)
	}
}

func TestReplayConfiguredKconfigCommandsRejectsStaleResult(t *testing.T) {
	root := t.TempDir()
	compiler := filepath.Join(root, "selected-compiler")
	linker := filepath.Join(root, "selected-linker")
	writeProbeTool(t, compiler, "#!/bin/sh\nexit 1\n")
	writeProbeTool(t, linker, "#!/bin/sh\nexit 0\n")
	supported := true
	command := configuredKconfigCommand(t, KconfigCommand{
		Kind:        KconfigCommandKindSuccess,
		Command:     "clang --unsupported",
		Environment: map[string]string{"CC": "clang", "LD": "ld.lld"},
		Inputs:      map[string]string{},
		Success:     &supported,
	})
	_, err := ReplayConfiguredKconfigCommands(
		context.Background(),
		[]KconfigCommand{command},
		KbuildGraphProbeTools{
			Compiler:   compiler,
			Linker:     linker,
			Shell:      "/bin/sh",
			SourceRoot: root,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "result mismatch") {
		t.Fatalf("ReplayConfiguredKconfigCommands() error = %v, want result mismatch", err)
	}
}

func TestEvaluateConfiguredKconfigCommandRunsFromSourceRoot(t *testing.T) {
	root := t.TempDir()
	writeProbeTool(t, filepath.Join(root, "compiler"), "#!/bin/sh\nexit 0\n")
	writeProbeTool(t, filepath.Join(root, "linker"), "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(root, "plugin", "include"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "plugin", "include", "plugin-version.h"),
		[]byte("#define PLUGIN_VERSION 1\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	got, err := EvaluateConfiguredKconfigCommand(
		context.Background(),
		`{ test -e plugin/include/plugin-version.h; } >/dev/null 2>&1 && echo "y" || echo "n"`,
		map[string]string{"CC": "clang", "LD": "ld.lld"},
		KbuildGraphProbeTools{
			Compiler:   filepath.Join(root, "compiler"),
			Linker:     filepath.Join(root, "linker"),
			Shell:      "/bin/sh",
			SourceRoot: root,
		},
	)
	if err != nil {
		t.Fatalf("EvaluateConfiguredKconfigCommand() failed: %v", err)
	}
	if got != "y" {
		t.Fatalf("EvaluateConfiguredKconfigCommand() = %q, want y", got)
	}
}

func TestRefreshConfiguredGraphProfileUpdatesResultsAndPreservesIDs(t *testing.T) {
	root := t.TempDir()
	compiler := filepath.Join(root, "selected-compiler")
	linker := filepath.Join(root, "selected-linker")
	writeProbeTool(t, compiler, `#!/bin/sh
case " $* " in
  *" -fgood "*) exit 0 ;;
esac
exit 1
`)
	writeProbeTool(t, linker, "#!/bin/sh\nexit 1\n")
	unsupported := false
	command := configuredKconfigCommand(t, KconfigCommand{
		Kind:        KconfigCommandKindSuccess,
		Command:     "clang -fgood",
		Environment: map[string]string{"CC": "clang", "LD": "ld.lld"},
		Inputs:      map[string]string{},
		Success:     &unsupported,
	})
	probeIdentity, err := NewKbuildGraphProbeIdentity(
		KbuildGraphProbeKindCCOption,
		"c",
		nil,
		[]string{"-fgood"},
		map[string]string{},
	)
	if err != nil {
		t.Fatal(err)
	}
	probe := KbuildGraphProbe{
		ID:            probeIdentity.ID,
		Kind:          probeIdentity.Kind,
		Language:      probeIdentity.Language,
		ContextArgv:   probeIdentity.ContextArgv,
		CandidateArgv: probeIdentity.CandidateArgv,
		Inputs:        probeIdentity.Inputs,
		Supported:     false,
	}
	profile := GraphProfile{
		Schema:         GraphProfileSchema,
		Architecture:   "x86_64",
		DriverContract: DriverContract,
		AnalysisIdentity: AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigCommands:   []KconfigCommand{command},
		KbuildGraphProbes: []KbuildGraphProbe{probe},
	}
	refreshed, err := RefreshConfiguredGraphProfile(
		context.Background(),
		profile,
		KbuildGraphProbeTools{
			Compiler:   compiler,
			Linker:     linker,
			Shell:      "/bin/sh",
			SourceRoot: root,
		},
	)
	if err != nil {
		t.Fatalf("RefreshConfiguredGraphProfile() failed: %v", err)
	}
	if refreshed.KconfigCommands[0].ID != command.ID ||
		refreshed.KconfigCommands[0].Success == nil ||
		!*refreshed.KconfigCommands[0].Success {
		t.Fatalf("refreshed command = %#v", refreshed.KconfigCommands[0])
	}
	if refreshed.KbuildGraphProbes[0].ID != probe.ID ||
		!refreshed.KbuildGraphProbes[0].Supported {
		t.Fatalf("refreshed probe = %#v", refreshed.KbuildGraphProbes[0])
	}
}

func writeProbeTool(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write probe tool: %v", err)
	}
}

func configuredKconfigCommand(t *testing.T, command KconfigCommand) KconfigCommand {
	t.Helper()
	identity, err := NewKconfigCommandIdentity(
		command.Kind,
		command.Command,
		command.Environment,
		command.Inputs,
	)
	if err != nil {
		t.Fatalf("NewKconfigCommandIdentity() failed: %v", err)
	}
	command.ID = identity.ID
	return command
}
