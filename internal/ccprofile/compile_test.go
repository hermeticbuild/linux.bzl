package ccprofile

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func validCommandTemplate(compiler string) CommandTemplate {
	return CommandTemplate{
		Schema:         CommandTemplateSchema,
		Architecture:   "x86_64",
		DriverContract: DriverContract,
		AnalysisIdentity: AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		Compiler: compiler,
		MutableArgv: []string{
			"-toolchain-keep",
			"-toolchain-drop",
			"-drop-everywhere",
			"__LINUX_BZL_KBUILD_FLAGS__",
			"-toolchain-suffix",
		},
		Environment:         map[string]string{},
		KbuildFlagsSentinel: "__LINUX_BZL_KBUILD_FLAGS__",
	}
}

func TestPrepareCompileArgvFiltersAllTopLevelMutableArguments(t *testing.T) {
	template := validCommandTemplate("/compiler")
	got, err := PrepareCompileArgv(
		template,
		"source.c",
		"output.o",
		[]string{
			"-DOBJECT_KEEP",
			"-DOBJECT_DROP",
			"-drop-everywhere",
			"@config-cflags.params",
		},
		[]string{
			"-toolchain-drop",
			"-DOBJECT_DROP",
			"-drop-everywhere",
		},
	)
	if err != nil {
		t.Fatalf("PrepareCompileArgv() failed: %v", err)
	}
	want := []string{
		"-toolchain-keep",
		"-DOBJECT_KEEP",
		"@config-cflags.params",
		"-toolchain-suffix",
		"-c",
		"source.c",
		"-o",
		"output.o",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v, want %#v", got, want)
	}
}

func TestPrepareCompileArgvRejectsSkeletonMutation(t *testing.T) {
	template := validCommandTemplate("/compiler")
	tests := []struct {
		name     string
		args     []string
		removals []string
	}{
		{name: "compile mode argument", args: []string{"-c"}},
		{name: "output argument", args: []string{"-oelsewhere.o"}},
		{name: "source argument", args: []string{"source.c"}},
		{name: "output path argument", args: []string{"output.o"}},
		{name: "sentinel argument", args: []string{template.KbuildFlagsSentinel}},
		{name: "remove compile mode", removals: []string{"-c"}},
		{name: "remove output option", removals: []string{"-o"}},
		{name: "remove source", removals: []string{"source.c"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := PrepareCompileArgv(
				template,
				"source.c",
				"output.o",
				test.args,
				test.removals,
			); err == nil {
				t.Fatal("PrepareCompileArgv() succeeded")
			}
		})
	}
	t.Run("template source argument", func(t *testing.T) {
		mutated := template
		mutated.MutableArgv = append([]string(nil), template.MutableArgv...)
		mutated.MutableArgv[0] = "source.c"
		if _, err := PrepareCompileArgv(
			mutated,
			"source.c",
			"output.o",
			nil,
			nil,
		); err == nil {
			t.Fatal("PrepareCompileArgv() succeeded")
		}
	})
}

func TestDecodeValidationStamp(t *testing.T) {
	valid := "profile_digest=" + strings.Repeat("a", 64) + "\n" +
		"compiler_identity_digest=" + strings.Repeat("b", 64) + "\n" +
		"validation_scope=compiler\n"
	stamp, err := DecodeValidationStamp([]byte(valid))
	if err != nil {
		t.Fatalf("DecodeValidationStamp() failed: %v", err)
	}
	if stamp.ValidationScope != "compiler" {
		t.Fatalf("validation scope = %q", stamp.ValidationScope)
	}
	for _, invalid := range []string{
		strings.TrimSuffix(valid, "\n"),
		strings.Replace(valid, "validation_scope=compiler", "validation_scope=all", 1),
		strings.Replace(valid, "profile_digest=", "unknown=", 1),
		valid + "extra=true\n",
	} {
		if _, err := DecodeValidationStamp([]byte(invalid)); err == nil {
			t.Fatalf("DecodeValidationStamp(%q) succeeded", invalid)
		}
	}
}

func TestDecodeCommandTemplateRejectsUnknownAndDuplicateFields(t *testing.T) {
	data, err := CanonicalCommandTemplateJSON(validCommandTemplate("/compiler"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCommandTemplate(data); err != nil {
		t.Fatalf("DecodeCommandTemplate() failed: %v", err)
	}
	unknown := strings.Replace(string(data), `"architecture":`, `"unknown": true, "architecture":`, 1)
	if _, err := DecodeCommandTemplate([]byte(unknown)); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("DecodeCommandTemplate(unknown) error = %v", err)
	}
	duplicate := strings.Replace(string(data), `"schema":`, `"schema": "duplicate", "schema":`, 1)
	if _, err := DecodeCommandTemplate([]byte(duplicate)); err == nil ||
		!strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("DecodeCommandTemplate(duplicate) error = %v", err)
	}
}

func TestCompileInvokesCompilerExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	compiler := filepath.Join(dir, "compiler")
	countPath := filepath.Join(dir, "count")
	argvPath := filepath.Join(dir, "argv")
	script := `#!/bin/sh
count=0
if [ -f "$COUNT_PATH" ]; then
  IFS= read -r count < "$COUNT_PATH"
fi
count=$((count + 1))
printf '%s\n' "$count" > "$COUNT_PATH"
printf '%s\n' "$@" > "$ARGV_PATH"
want_output=
output=
for arg in "$@"; do
  if [ "$want_output" = 1 ]; then
    output=$arg
    want_output=
  elif [ "$arg" = "-o" ]; then
    want_output=1
  fi
done
[ -n "$output" ] || exit 9
: > "$output"
`
	if err := os.WriteFile(compiler, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	template := validCommandTemplate(compiler)
	template.Environment = map[string]string{
		"ARGV_PATH":  argvPath,
		"COUNT_PATH": countPath,
	}
	source := filepath.Join(dir, "source.c")
	output := filepath.Join(dir, "output.o")
	if err := os.WriteFile(source, []byte("int value;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Compile(
		context.Background(),
		template,
		source,
		output,
		[]string{"-DOBJECT_KEEP"},
		[]string{"-toolchain-drop", "-drop-everywhere"},
		os.Stdout,
		os.Stderr,
	); err != nil {
		t.Fatalf("Compile() failed: %v", err)
	}
	count, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(count) != "1\n" {
		t.Fatalf("compiler invocation count = %q", count)
	}
	argvData, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(argvData), "\n"), "\n")
	want := []string{
		"-toolchain-keep",
		"-DOBJECT_KEEP",
		"-toolchain-suffix",
		"-c",
		source,
		"-o",
		output,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compiler argv = %#v, want %#v", got, want)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("compiler did not create output: %v", err)
	}
}
