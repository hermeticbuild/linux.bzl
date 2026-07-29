package ccprofile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvaluateStructuralProbeUsesLinuxSemantics(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "commands.log")
	t.Setenv("PROBE_LOG", logPath)
	compiler := filepath.Join(dir, "clang")
	linker := filepath.Join(dir, "ld.lld")
	writeProbeTool := func(path, name string) {
		t.Helper()
		script := "#!/bin/sh\n" +
			"printf '%s:%s\\n' '" + name + "' \"$*\" >> \"$PROBE_LOG\"\n" +
			"case \" $* \" in *' -funsupported '*) exit 1;; esac\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatalf("WriteFile(%q) failed: %v", path, err)
		}
	}
	writeProbeTool(compiler, "compiler")
	writeProbeTool(linker, "linker")

	profile := Profile{
		AnalysisIdentity: AnalysisIdentity{
			TargetGNUSystemName: "aarch64-unknown-linux-gnu",
		},
	}
	tools := StructuralProbeTools{
		Compiler:   compiler,
		Linker:     linker,
		SourceRoot: filepath.Join(dir, "linux"),
	}
	tests := []struct {
		probe StructuralProbe
		want  bool
	}{
		{
			probe: StructuralProbe{
				Kind:       "cc-option",
				Language:   "c",
				PrefixArgv: []string{"-Werror", "-I" + StructuralProbeSourceRoot + "/include"},
				Argv:       []string{"-Wno-unused"},
			},
			want: true,
		},
		{
			probe: StructuralProbe{
				Kind:     "cc-disable-warning",
				Language: "c",
				Argv:     []string{"packed"},
			},
			want: true,
		},
		{
			probe: StructuralProbe{
				Kind:     "cc-option-yn",
				Language: "c",
				Argv:     []string{"-funsupported"},
			},
			want: false,
		},
		{
			probe: StructuralProbe{
				Kind:     "as-option",
				Language: "asm",
				Argv:     []string{"-Wa,--fatal-warnings"},
			},
			want: true,
		},
		{
			probe: StructuralProbe{
				Kind:     "ld-option",
				Language: "link",
				Argv:     []string{"--build-id"},
			},
			want: true,
		},
	}
	for index := range tests {
		tests[index].probe.ID = StructuralProbeID(tests[index].probe)
		got, err := EvaluateStructuralProbe(
			context.Background(),
			profile,
			tests[index].probe,
			tools,
		)
		if err != nil {
			t.Fatalf("EvaluateStructuralProbe(%d) failed: %v", index, err)
		}
		if got != tests[index].want {
			t.Errorf("EvaluateStructuralProbe(%d) = %t, want %t", index, got, tests[index].want)
		}
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile() failed: %v", err)
	}
	for _, want := range []string{
		"--target=aarch64-unknown-linux-gnu",
		"-I" + filepath.ToSlash(filepath.Join(dir, "linux")) + "/include",
		"-Wunused",
		"-Wpacked",
		"-x assembler-with-cpp",
		"linker:--build-id -v",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("probe commands omit %q:\n%s", want, log)
		}
	}
	if strings.Contains(string(log), "-Wno-unused") {
		t.Fatalf("cc-option warning probe did not enable the warning:\n%s", log)
	}
}

func TestEvaluateStructuralProbeRejectsWrongID(t *testing.T) {
	_, err := EvaluateStructuralProbe(
		context.Background(),
		Profile{},
		StructuralProbe{
			ID:       strings.Repeat("0", 64),
			Kind:     "cc-option",
			Language: "c",
			Argv:     []string{"-fno-pic"},
		},
		StructuralProbeTools{
			Compiler: "clang",
			Linker:   "ld.lld",
		},
	)
	if err == nil || !strings.Contains(err.Error(), "request ID") {
		t.Fatalf("EvaluateStructuralProbe() error = %v, want request ID rejection", err)
	}
}
