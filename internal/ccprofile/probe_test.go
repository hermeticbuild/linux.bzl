package ccprofile

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReplayConfiguredKconfigCommandsUsesSelectedTools(t *testing.T) {
	root := t.TempDir()
	compiler := filepath.Join(root, "selected-compiler")
	linker := filepath.Join(root, "selected-linker")
	writeProbeTool(t, compiler, `#!/bin/sh
case " $* " in
  *" --supported "*) exit 0 ;;
  *" --identity "*) printf 'configured\ncompiler\n'; exit 0 ;;
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
			CommandTemplate: testProbeCommandTemplate(
				compiler,
				"x86_64-unknown-linux-gnu",
				nil,
			),
			Archiver:   compiler,
			Coreutils:  compiler,
			Grep:       compiler,
			Linker:     linker,
			NM:         compiler,
			Objcopy:    compiler,
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

func TestReplayConfiguredKconfigCommandsUsesSelectedBinutils(t *testing.T) {
	root := t.TempDir()
	compiler := filepath.Join(root, "compiler", "clang")
	linker := filepath.Join(root, "linker", "ld.lld")
	archiver := filepath.Join(root, "archiver", "llvm-ar")
	nm := filepath.Join(root, "nm", "llvm-nm")
	objcopy := filepath.Join(root, "objcopy", "llvm-objcopy")
	coreutils := filepath.Join(root, "coreutils")
	grep := filepath.Join(root, "grep")
	for _, directory := range []string{
		filepath.Dir(compiler),
		filepath.Dir(linker),
		filepath.Dir(archiver),
		filepath.Dir(nm),
		filepath.Dir(objcopy),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeProbeTool(t, compiler, "#!/bin/sh\nexit 0\n")
	writeProbeTool(t, linker, "#!/bin/sh\nexit 0\n")
	writeProbeTool(t, archiver, "#!/bin/sh\nprintf 'llvm selected archiver\\nignored\\n'\n")
	writeProbeTool(t, nm, "#!/bin/sh\nprintf 'llvm selected nm\\nignored\\n'\n")
	writeProbeTool(t, objcopy, "#!/bin/sh\nprintf 'llvm selected objcopy\\nignored\\n'\n")
	writeProbeTool(t, coreutils, `#!/bin/sh
case "${0##*/}" in
  head)
    IFS= read -r line || exit 0
    printf '%s\n' "$line"
    ;;
  *)
    exit 64
    ;;
esac
`)
	writeProbeTool(t, grep, `#!/bin/sh
IFS= read -r line || line=
case " $* " in
  *" -qv "*)
    case "$line" in *llvm*) exit 1;; *) exit 0;; esac
    ;;
  *)
    case "$line" in *llvm*) exit 0;; *) exit 1;; esac
    ;;
esac
`)
	environment := map[string]string{
		"AR":      "llvm-ar",
		"CC":      "clang",
		"LD":      "ld.lld",
		"NM":      "llvm-nm",
		"OBJCOPY": "llvm-objcopy",
	}
	supported := true
	unsupported := false
	commands := []KconfigCommand{
		configuredKconfigCommand(t, KconfigCommand{
			Kind:        KconfigCommandKindSuccess,
			Command:     "llvm-ar --help | head -n 1 | grep -qi llvm",
			Environment: environment,
			Inputs:      map[string]string{},
			Success:     &supported,
		}),
		configuredKconfigCommand(t, KconfigCommand{
			Kind:        KconfigCommandKindSuccess,
			Command:     "llvm-nm --help | head -n 1 | grep -qi llvm",
			Environment: environment,
			Inputs:      map[string]string{},
			Success:     &supported,
		}),
		configuredKconfigCommand(t, KconfigCommand{
			Kind:        KconfigCommandKindSuccess,
			Command:     "llvm-objcopy --version | head -n1 | grep -qv llvm",
			Environment: environment,
			Inputs:      map[string]string{},
			Success:     &unsupported,
		}),
	}
	t.Setenv("PATH", filepath.Join(root, "unusable-host-path"))
	replayed, err := ReplayConfiguredKconfigCommands(
		context.Background(),
		commands,
		KbuildGraphProbeTools{
			CommandTemplate: testProbeCommandTemplate(
				compiler,
				"x86_64-unknown-linux-gnu",
				nil,
			),
			Archiver:   archiver,
			Coreutils:  coreutils,
			Grep:       grep,
			Linker:     linker,
			NM:         nm,
			Objcopy:    objcopy,
			Shell:      "/bin/sh",
			SourceRoot: root,
		},
	)
	if err != nil {
		t.Fatalf("ReplayConfiguredKconfigCommands() failed: %v", err)
	}
	if replayed != len(commands) {
		t.Fatalf("replayed commands = %d, want %d", replayed, len(commands))
	}
}

func TestIsConfiguredKconfigCommandRecognizesToolAliases(t *testing.T) {
	environment := map[string]string{
		"AR":      "selected-ar",
		"CC":      "selected-cc",
		"LD":      "selected-ld",
		"NM":      "selected-nm",
		"OBJCOPY": "selected-objcopy",
	}
	for _, command := range []string{
		"$AR --help",
		"${NM} --help",
		"$OBJCOPY --version",
		"selected-ar --help",
		"selected-cc --version",
		"selected-ld --version",
		"selected-nm --help",
		"selected-objcopy --version",
	} {
		if !isConfiguredKconfigCommand(command, environment) {
			t.Errorf("isConfiguredKconfigCommand(%q) = false, want true", command)
		}
	}
	if isConfiguredKconfigCommand("echo selected-archive", environment) {
		t.Fatal("isConfiguredKconfigCommand() matched an alias substring")
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
			CommandTemplate: testProbeCommandTemplate(
				compiler,
				"x86_64-unknown-linux-gnu",
				nil,
			),
			Archiver:   compiler,
			Coreutils:  compiler,
			Grep:       compiler,
			Linker:     linker,
			NM:         compiler,
			Objcopy:    compiler,
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
			CommandTemplate: testProbeCommandTemplate(
				filepath.Join(root, "compiler"),
				"x86_64-unknown-linux-gnu",
				nil,
			),
			Archiver:   filepath.Join(root, "compiler"),
			Coreutils:  filepath.Join(root, "compiler"),
			Grep:       filepath.Join(root, "compiler"),
			Linker:     filepath.Join(root, "linker"),
			NM:         filepath.Join(root, "compiler"),
			Objcopy:    filepath.Join(root, "compiler"),
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
			CommandTemplate: testProbeCommandTemplate(
				compiler,
				profile.AnalysisIdentity.TargetGNUSystemName,
				nil,
			),
			Archiver:   compiler,
			Coreutils:  compiler,
			Grep:       compiler,
			Linker:     linker,
			NM:         compiler,
			Objcopy:    compiler,
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

func TestEvaluateKbuildGraphProbeUsesCommandTemplatePrefix(t *testing.T) {
	root := t.TempDir()
	compiler := filepath.Join(root, "selected-compiler")
	logPath := filepath.Join(root, "compiler.argv")
	writeProbeTool(t, compiler, `#!/bin/sh
printf '%s\n' "$@" > "$LOG_PATH"
exit 0
`)
	template := testProbeCommandTemplate(
		compiler,
		"x86_64-unknown-linux-gnu",
		map[string]string{"LOG_PATH": logPath},
	)
	template.MutableArgv = []string{
		"--target=x86_64-linux-gnu",
		"-fbefore",
		"-nostdinc",
		"-fintegrated-as",
		template.KbuildFlagsSentinel,
	}
	request, err := NewKbuildGraphProbeIdentity(
		KbuildGraphProbeKindCCOption,
		"c",
		[]string{"-Werror"},
		[]string{"-fcandidate"},
		map[string]string{},
	)
	if err != nil {
		t.Fatal(err)
	}
	supported, err := EvaluateKbuildGraphProbe(
		context.Background(),
		request,
		KbuildGraphProbeTools{
			CommandTemplate: template,
			Linker:          compiler,
			SourceRoot:      root,
		},
	)
	if err != nil {
		t.Fatalf("EvaluateKbuildGraphProbe() failed: %v", err)
	}
	if !supported {
		t.Fatal("EvaluateKbuildGraphProbe() = false, want true")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Fields(string(data))
	before := slices.Index(argv, "-fbefore")
	context := slices.Index(argv, "-Werror")
	candidate := slices.Index(argv, "-fcandidate")
	if before < 0 || context <= before || candidate <= context {
		t.Fatalf("compiler argv does not preserve template insertion order:\n%s", data)
	}
}

func TestConfiguredKbuildGraphProbeReplaySkipsHistoricalSourceInputs(t *testing.T) {
	root := t.TempDir()
	inputPath := filepath.Join(root, "scripts", "probe.h")
	if err := os.MkdirAll(filepath.Dir(inputPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	compiler := filepath.Join(root, "selected-compiler")
	linker := filepath.Join(root, "selected-linker")
	writeProbeTool(t, compiler, "#!/bin/sh\nexit 0\n")
	writeProbeTool(t, linker, "#!/bin/sh\nexit 1\n")
	identity, err := NewKbuildGraphProbeIdentity(
		KbuildGraphProbeKindCCOption,
		"c",
		nil,
		[]string{"-fgood"},
		map[string]string{
			"scripts/probe.h": fmt.Sprintf(
				"%x",
				sha256.Sum256([]byte("recorded")),
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	probe := KbuildGraphProbe{
		ID:            identity.ID,
		Kind:          identity.Kind,
		Language:      identity.Language,
		ContextArgv:   identity.ContextArgv,
		CandidateArgv: identity.CandidateArgv,
		Inputs:        identity.Inputs,
		Supported:     false,
	}
	analysisIdentity := testAnalysisIdentity("x86_64-unknown-linux-gnu")
	tools := KbuildGraphProbeTools{
		CommandTemplate: testProbeCommandTemplate(
			compiler,
			analysisIdentity.TargetGNUSystemName,
			nil,
		),
		Archiver:   compiler,
		Coreutils:  compiler,
		Grep:       compiler,
		Linker:     linker,
		NM:         compiler,
		Objcopy:    compiler,
		Shell:      "/bin/sh",
		SourceRoot: root,
	}

	if _, err := ReplayConfiguredKbuildGraphProbes(
		context.Background(),
		[]KbuildGraphProbe{probe},
		tools,
	); err == nil || !strings.Contains(err.Error(), "inputs do not match") {
		t.Fatalf(
			"ReplayConfiguredKbuildGraphProbes() error = %v, want input mismatch",
			err,
		)
	}
	replayed, err := ReplayMatchingConfiguredKbuildGraphProbes(
		context.Background(),
		[]KbuildGraphProbe{probe},
		tools,
	)
	if err != nil {
		t.Fatalf("ReplayMatchingConfiguredKbuildGraphProbes() failed: %v", err)
	}
	if replayed != 0 {
		t.Fatalf("matching-source replayed probes = %d, want 0", replayed)
	}

	profile := GraphProfile{
		Schema:            GraphProfileSchema,
		Architecture:      "x86_64",
		DriverContract:    DriverContract,
		AnalysisIdentity:  analysisIdentity,
		KbuildGraphProbes: []KbuildGraphProbe{probe},
		KconfigCommands:   []KconfigCommand{},
	}
	refreshed, err := RefreshConfiguredGraphProfile(
		context.Background(),
		profile,
		tools,
	)
	if err != nil {
		t.Fatalf("RefreshConfiguredGraphProfile() failed: %v", err)
	}
	if refreshed.KbuildGraphProbes[0].Supported {
		t.Fatalf(
			"historical Kbuild graph probe result changed: %#v",
			refreshed.KbuildGraphProbes[0],
		)
	}
}

func TestReplayConfiguredKconfigCommandsUsesHermeticCoreutils(t *testing.T) {
	root := t.TempDir()
	compiler := filepath.Join(root, "selected-compiler")
	linker := filepath.Join(root, "selected-linker")
	coreutils := filepath.Join(root, "coreutils")
	writeProbeTool(t, compiler, "#!/bin/sh\nexit 0\n")
	writeProbeTool(t, linker, "#!/bin/sh\nexit 0\n")
	writeProbeTool(t, coreutils, `#!/bin/sh
case "${0##*/}" in
  dirname)
    value=${1%/*}
    [ "$value" = "$1" ] && value=.
    printf '%s\n' "$value"
    ;;
  *)
    exit 64
    ;;
esac
`)
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProbeTool(
		t,
		filepath.Join(root, "scripts", "cc-path.sh"),
		"#!/bin/sh\ndirname path/to/file\n",
	)
	t.Setenv("PATH", filepath.Join(root, "missing-host-path"))
	stdout := "path/to"
	command := configuredKconfigCommand(t, KconfigCommand{
		Kind:        KconfigCommandKindStdout,
		Command:     GraphProfileSourceRoot + "/scripts/cc-path.sh",
		Environment: map[string]string{"CC": "clang", "LD": "ld.lld"},
		Inputs:      map[string]string{},
		Stdout:      &stdout,
	})
	replayed, err := ReplayConfiguredKconfigCommands(
		context.Background(),
		[]KconfigCommand{command},
		KbuildGraphProbeTools{
			CommandTemplate: testProbeCommandTemplate(
				compiler,
				"x86_64-unknown-linux-gnu",
				nil,
			),
			Archiver:   compiler,
			Coreutils:  coreutils,
			Grep:       compiler,
			Linker:     linker,
			NM:         compiler,
			Objcopy:    compiler,
			Shell:      "/bin/sh",
			SourceRoot: root,
		},
	)
	if err != nil {
		t.Fatalf("ReplayConfiguredKconfigCommands() failed: %v", err)
	}
	if replayed != 1 {
		t.Fatalf("replayed commands = %d, want 1", replayed)
	}
}

func TestReplayConfiguredKconfigCommandsInjectsClangTarget(t *testing.T) {
	root := t.TempDir()
	compiler := filepath.Join(root, "selected-compiler")
	linker := filepath.Join(root, "selected-linker")
	logPath := filepath.Join(root, "compiler.argv")
	writeProbeTool(t, compiler, "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$LOG_PATH\"\nexit 0\n")
	writeProbeTool(t, linker, "#!/bin/sh\nexit 0\n")
	supported := true
	command := configuredKconfigCommand(t, KconfigCommand{
		Kind:        KconfigCommandKindSuccess,
		Command:     "$CC --probe",
		Environment: map[string]string{"CC": "clang", "LD": "ld.lld"},
		Inputs:      map[string]string{},
		Success:     &supported,
	})
	_, err := ReplayConfiguredKconfigCommands(
		context.Background(),
		[]KconfigCommand{command},
		KbuildGraphProbeTools{
			CommandTemplate: testProbeCommandTemplate(
				compiler,
				"aarch64-unknown-linux-gnu",
				map[string]string{"LOG_PATH": logPath},
			),
			Archiver:   compiler,
			Coreutils:  compiler,
			Grep:       compiler,
			Linker:     linker,
			NM:         compiler,
			Objcopy:    compiler,
			Shell:      "/bin/sh",
			SourceRoot: root,
		},
	)
	if err != nil {
		t.Fatalf("ReplayConfiguredKconfigCommands() failed: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{
		"--target=aarch64-linux-gnu",
		"-nostdinc",
		"-fintegrated-as",
		"--probe",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("configured compiler argv = %#v, want %#v", got, want)
	}
}

func TestReplayConfiguredKconfigCommandsAbsolutizesTemplateInputs(t *testing.T) {
	executionRoot := t.TempDir()
	t.Chdir(executionRoot)
	sourceRoot := filepath.Join(executionRoot, "source")
	includeDir := filepath.Join(executionRoot, "toolchain", "include")
	for _, directory := range []string{sourceRoot, includeDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	compiler := filepath.Join(executionRoot, "selected-compiler")
	linker := filepath.Join(executionRoot, "selected-linker")
	logPath := filepath.Join(executionRoot, "compiler.argv")
	writeProbeTool(t, compiler, "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$LOG_PATH\"\nexit 0\n")
	writeProbeTool(t, linker, "#!/bin/sh\nexit 0\n")
	template := testProbeCommandTemplate(
		compiler,
		"x86_64-unknown-linux-gnu",
		map[string]string{"LOG_PATH": logPath},
	)
	template.MutableArgv = []string{
		"--target=x86_64-linux-gnu",
		"-isystem",
		"toolchain/include",
		"-nostdinc",
		"-fintegrated-as",
		template.KbuildFlagsSentinel,
	}
	supported := true
	command := configuredKconfigCommand(t, KconfigCommand{
		Kind:        KconfigCommandKindSuccess,
		Command:     "$CC --probe",
		Environment: map[string]string{"CC": "clang", "LD": "ld.lld"},
		Inputs:      map[string]string{},
		Success:     &supported,
	})
	tools := KbuildGraphProbeTools{
		CommandTemplate: template,
		Archiver:        compiler,
		Coreutils:       compiler,
		Grep:            compiler,
		Linker:          linker,
		NM:              compiler,
		Objcopy:         compiler,
		Shell:           "/bin/sh",
		SourceRoot:      sourceRoot,
	}
	if _, err := ReplayConfiguredKconfigCommands(
		context.Background(),
		[]KconfigCommand{command},
		tools,
	); err != nil {
		t.Fatalf("ReplayConfiguredKconfigCommands() failed: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if !slices.Contains(got, includeDir) {
		t.Fatalf("configured compiler argv retained cwd-relative template input: %#v", got)
	}
}

func TestRefreshConfiguredKconfigCommandsRequiresMatchingInputs(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "scripts", "probe.sh")
	if err := os.MkdirAll(filepath.Dir(input), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	compiler := filepath.Join(root, "selected-compiler")
	linker := filepath.Join(root, "selected-linker")
	writeProbeTool(t, compiler, "#!/bin/sh\nexit 1\n")
	writeProbeTool(t, linker, "#!/bin/sh\nexit 0\n")
	recorded := true
	command := configuredKconfigCommand(t, KconfigCommand{
		Kind:        KconfigCommandKindSuccess,
		Command:     "$CC --probe",
		Environment: map[string]string{"CC": "clang", "LD": "ld.lld"},
		Inputs: map[string]string{
			"scripts/probe.sh": fmt.Sprintf("%x", sha256.Sum256([]byte("recorded"))),
		},
		Success: &recorded,
	})
	refreshed, replayed, err := RefreshConfiguredKconfigCommands(
		context.Background(),
		[]KconfigCommand{command},
		KbuildGraphProbeTools{
			CommandTemplate: testProbeCommandTemplate(
				compiler,
				"x86_64-unknown-linux-gnu",
				nil,
			),
			Archiver:   compiler,
			Coreutils:  compiler,
			Grep:       compiler,
			Linker:     linker,
			NM:         compiler,
			Objcopy:    compiler,
			Shell:      "/bin/sh",
			SourceRoot: root,
		},
	)
	if err != nil {
		t.Fatalf("RefreshConfiguredKconfigCommands() failed: %v", err)
	}
	if replayed != 0 {
		t.Fatalf("replayed commands = %d, want 0", replayed)
	}
	if refreshed[0].Success == nil || !*refreshed[0].Success {
		t.Fatalf("mismatched-input result changed: %#v", refreshed[0])
	}
	_, err = ReplayConfiguredKconfigCommands(
		context.Background(),
		[]KconfigCommand{command},
		KbuildGraphProbeTools{
			CommandTemplate: testProbeCommandTemplate(
				compiler,
				"x86_64-unknown-linux-gnu",
				nil,
			),
			Archiver:   compiler,
			Coreutils:  compiler,
			Grep:       compiler,
			Linker:     linker,
			NM:         compiler,
			Objcopy:    compiler,
			Shell:      "/bin/sh",
			SourceRoot: root,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "inputs do not match") {
		t.Fatalf("ReplayConfiguredKconfigCommands() error = %v, want input mismatch", err)
	}
	replayed, err = ReplayMatchingConfiguredKconfigCommands(
		context.Background(),
		[]KconfigCommand{command},
		KbuildGraphProbeTools{
			CommandTemplate: testProbeCommandTemplate(
				compiler,
				"x86_64-unknown-linux-gnu",
				nil,
			),
			Archiver:   compiler,
			Coreutils:  compiler,
			Grep:       compiler,
			Linker:     linker,
			NM:         compiler,
			Objcopy:    compiler,
			Shell:      "/bin/sh",
			SourceRoot: root,
		},
	)
	if err != nil {
		t.Fatalf("ReplayMatchingConfiguredKconfigCommands() failed: %v", err)
	}
	if replayed != 0 {
		t.Fatalf("matching-source replayed commands = %d, want 0", replayed)
	}
}

func testAnalysisIdentity(target string) AnalysisIdentity {
	return AnalysisIdentity{
		Compiler:            "clang",
		TargetGNUSystemName: target,
	}
}

func testProbeCommandTemplate(
	compiler string,
	target string,
	environment map[string]string,
) CommandTemplate {
	architecture := "x86_64"
	if strings.HasPrefix(target, "aarch64") {
		architecture = "aarch64"
	}
	canonicalTarget := strings.Replace(target, "-unknown-", "-", 1)
	const sentinel = "__LINUX_BZL_TEST_KBUILD_FLAGS__"
	return CommandTemplate{
		Schema:           CommandTemplateSchema,
		Architecture:     architecture,
		DriverContract:   DriverContract,
		AnalysisIdentity: testAnalysisIdentity(target),
		Compiler:         compiler,
		MutableArgv: []string{
			"--target=" + canonicalTarget,
			"-nostdinc",
			"-fintegrated-as",
			sentinel,
		},
		Environment:         cloneStringMap(environment),
		KbuildFlagsSentinel: sentinel,
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
