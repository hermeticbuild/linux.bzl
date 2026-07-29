package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/ccprofile"
)

func testProfile(t *testing.T, dir string) string {
	t.Helper()
	probe := ccprofile.StructuralProbe{
		Kind:      "cc-option",
		Language:  "c",
		Argv:      []string{"-fno-omit-frame-pointer"},
		Supported: true,
	}
	probe.ID = ccprofile.StructuralProbeID(probe)
	profile := ccprofile.Profile{
		Schema:         ccprofile.Schema,
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigIdentity: ccprofile.KconfigIdentity{
			CCName:        "Clang",
			CCVersion:     220108,
			CCVersionText: "clang version 22.1.8",
			ASName:        "LLVM",
			LDName:        "LLD",
			LDVersion:     220108,
			CanLink:       true,
			BuiltinMacros: map[string]string{},
		},
		StructuralProbes: []ccprofile.StructuralProbe{probe},
	}
	data, err := ccprofile.CanonicalJSON(profile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "profile.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckAndCompare(t *testing.T) {
	dir := t.TempDir()
	profile := testProfile(t, dir)
	canonical := filepath.Join(dir, "canonical.json")
	digest := filepath.Join(dir, "digest")
	if err := run([]string{
		"check",
		"-profile", profile,
		"-canonical_out", canonical,
		"-digest_out", digest,
	}); err != nil {
		t.Fatalf("check failed: %v", err)
	}
	digestData, err := os.ReadFile(digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(digestData))) != 64 {
		t.Fatalf("digest = %q", digestData)
	}
	stamp := filepath.Join(dir, "stamp")
	if err := run([]string{
		"compare",
		"-expected", profile,
		"-actual", canonical,
		"-out", stamp,
	}); err != nil {
		t.Fatalf("compare failed: %v", err)
	}
	stampData, err := os.ReadFile(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(stampData), "profile_digest=") {
		t.Fatalf("stamp = %q", stampData)
	}
}

func TestInspectAndValidate(t *testing.T) {
	dir := t.TempDir()
	compiler := filepath.Join(dir, "clang")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf '%s\n' 'clang version 22.1.8'
  exit 0
fi
printf '%s\n' \
  '#define __GNUC__ 4' \
  '#define __GNUC_MINOR__ 2' \
  '#define __GNUC_PATCHLEVEL__ 1' \
  '#define __clang_major__ 22' \
  '#define __clang_minor__ 1' \
  '#define __clang_patchlevel__ 8' \
  '#define __SIZEOF_INT128__ 16'
`
	if err := os.WriteFile(compiler, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sourceSentinel := "__LINUX_BZL_SOURCE__"
	outputSentinel := "__LINUX_BZL_OUTPUT__"
	flagsSentinel := "__LINUX_BZL_KBUILD_FLAGS__"
	template := filepath.Join(dir, "template.json")
	identity := filepath.Join(dir, "identity.json")
	if err := run([]string{
		"inspect",
		"-architecture", "x86_64",
		"-analysis_compiler", "clang",
		"-analysis_target_gnu_system_name", "x86_64-unknown-linux-gnu",
		"-compiler", compiler,
		"-source_sentinel", sourceSentinel,
		"-output_sentinel", outputSentinel,
		"-kbuild_flags_sentinel", flagsSentinel,
		"-compile_arg=--target=x86_64-unknown-linux-gnu",
		"-compile_arg=" + flagsSentinel,
		"-compile_arg=-c",
		"-compile_arg=" + sourceSentinel,
		"-compile_arg=-o",
		"-compile_arg=" + outputSentinel,
		"-compile_env=LC_ALL=C",
		"-template_out", template,
		"-identity_out", identity,
	}); err != nil {
		t.Fatalf("inspect failed: %v", err)
	}
	templateData, err := os.ReadFile(template)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(templateData), sourceSentinel) ||
		strings.Contains(string(templateData), outputSentinel) {
		t.Fatalf("command template retained structural sentinel:\n%s", templateData)
	}
	if !strings.Contains(string(templateData), flagsSentinel) {
		t.Fatalf("command template omitted flags sentinel:\n%s", templateData)
	}

	stamp := filepath.Join(dir, "validated")
	if err := run([]string{
		"validate",
		"-profile", testProfile(t, dir),
		"-identity", identity,
		"-out", stamp,
	}); err != nil {
		t.Fatalf("validate failed: %v", err)
	}
	stampData, err := os.ReadFile(stamp)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"profile_digest=",
		"compiler_identity_digest=",
		"validation_scope=compiler\n",
	} {
		if !strings.Contains(string(stampData), line) {
			t.Fatalf("validation stamp omits %q:\n%s", line, stampData)
		}
	}
}

func TestProbePopulatesCanonicalProfile(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(tool) failed: %v", err)
	}
	request := ccprofile.StructuralProbe{
		Kind:       "cc-option",
		Language:   "c",
		PrefixArgv: []string{"-Werror"},
		Argv:       []string{"-fno-pic"},
	}
	request.ID = ccprofile.StructuralProbeID(request)
	requestData, err := json.MarshalIndent(structuralProbeRequestManifest{
		Schema: structuralProbeRequestsSchema,
		StructuralProbes: []structuralProbeRequest{{
			ID:         request.ID,
			Kind:       request.Kind,
			Language:   request.Language,
			PrefixArgv: request.PrefixArgv,
			Argv:       request.Argv,
		}},
	}, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent(requests) failed: %v", err)
	}
	requestsPath := filepath.Join(dir, "requests.json")
	if err := os.WriteFile(requestsPath, append(requestData, '\n'), 0o644); err != nil {
		t.Fatalf("WriteFile(requests) failed: %v", err)
	}
	out := filepath.Join(dir, "populated.json")
	if err := run([]string{
		"probe",
		"-profile", testProfile(t, dir),
		"-requests", requestsPath,
		"-compiler", tool,
		"-linker", tool,
		"-out", out,
	}); err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	profile, err := readProfile(out)
	if err != nil {
		t.Fatalf("readProfile(output) failed: %v", err)
	}
	if got, want := len(profile.StructuralProbes), 2; got != want {
		t.Fatalf("probe count = %d, want %d", got, want)
	}
	found := false
	for _, probe := range profile.StructuralProbes {
		if probe.ID == request.ID {
			found = true
			if !probe.Supported {
				t.Fatal("successful structural probe was recorded as unsupported")
			}
		}
	}
	if !found {
		t.Fatalf("populated profile omits request %s", request.ID)
	}
}

func TestInspectRejectsAmbiguousSkeleton(t *testing.T) {
	dir := t.TempDir()
	err := run([]string{
		"inspect",
		"-architecture", "x86_64",
		"-analysis_compiler", "clang",
		"-analysis_target_gnu_system_name", "x86_64-unknown-linux-gnu",
		"-compiler", filepath.Join(dir, "unused"),
		"-source_sentinel", "SOURCE",
		"-output_sentinel", "OUTPUT",
		"-kbuild_flags_sentinel", "FLAGS",
		"-compile_arg=-c",
		"-compile_arg=SOURCE",
		"-compile_arg=-o",
		"-compile_arg=OUTPUT",
		"-compile_arg=FLAGS",
		"-compile_arg=SOURCE",
		"-template_out", filepath.Join(dir, "template"),
		"-identity_out", filepath.Join(dir, "identity"),
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one source") {
		t.Fatalf("inspect error = %v", err)
	}
}

func TestCompileUsesValidatedTemplateOnce(t *testing.T) {
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
	sentinel := "__LINUX_BZL_KBUILD_FLAGS__"
	template := ccprofile.CommandTemplate{
		Schema:         ccprofile.CommandTemplateSchema,
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		Compiler: compiler,
		MutableArgv: []string{
			"-toolchain-keep",
			"-toolchain-drop",
			sentinel,
			"-drop-everywhere",
		},
		Environment: map[string]string{
			"ARGV_PATH":  argvPath,
			"COUNT_PATH": countPath,
		},
		KbuildFlagsSentinel: sentinel,
	}
	templateData, err := ccprofile.CanonicalCommandTemplateJSON(template)
	if err != nil {
		t.Fatal(err)
	}
	templatePath := filepath.Join(dir, "template.json")
	if err := os.WriteFile(templatePath, templateData, 0o644); err != nil {
		t.Fatal(err)
	}
	validationPath := writeValidationStamp(t, dir)
	source := filepath.Join(dir, "source.c")
	output := filepath.Join(dir, "output.o")
	if err := os.WriteFile(source, []byte("int value;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"compile",
		"-template", templatePath,
		"-validation", validationPath,
		"-source", source,
		"-output", output,
		"-arg=-DOBJECT_KEEP",
		"-arg=-DOBJECT_DROP",
		"-arg=-drop-everywhere",
		"-arg=@config-cflags.params",
		"-remove=-toolchain-drop",
		"-remove=-DOBJECT_DROP",
		"-remove=-drop-everywhere",
	}); err != nil {
		t.Fatalf("compile failed: %v", err)
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
		"@config-cflags.params",
		"-c",
		source,
		"-o",
		output,
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("compiler argv = %#v, want %#v", got, want)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("compile output: %v", err)
	}
}

func TestCompileRejectsInvalidStampBeforeInvocation(t *testing.T) {
	dir := t.TempDir()
	countPath := filepath.Join(dir, "count")
	compiler := filepath.Join(dir, "compiler")
	script := `#!/bin/sh
printf '%s\n' invoked > "$COUNT_PATH"
`
	if err := os.WriteFile(compiler, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := "__LINUX_BZL_KBUILD_FLAGS__"
	template := ccprofile.CommandTemplate{
		Schema:         ccprofile.CommandTemplateSchema,
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		Compiler:            compiler,
		MutableArgv:         []string{sentinel},
		Environment:         map[string]string{"COUNT_PATH": countPath},
		KbuildFlagsSentinel: sentinel,
	}
	templateData, err := ccprofile.CanonicalCommandTemplateJSON(template)
	if err != nil {
		t.Fatal(err)
	}
	templatePath := filepath.Join(dir, "template.json")
	if err := os.WriteFile(templatePath, templateData, 0o644); err != nil {
		t.Fatal(err)
	}
	validationPath := filepath.Join(dir, "invalid.validation")
	if err := os.WriteFile(validationPath, []byte("validation_scope=compiler\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = run([]string{
		"compile",
		"-template", templatePath,
		"-validation", validationPath,
		"-source", filepath.Join(dir, "source.c"),
		"-output", filepath.Join(dir, "output.o"),
	})
	if err == nil || !strings.Contains(err.Error(), "validation stamp") {
		t.Fatalf("compile error = %v", err)
	}
	if _, err := os.Stat(countPath); !os.IsNotExist(err) {
		t.Fatalf("compiler was invoked despite invalid stamp: %v", err)
	}
}

func writeValidationStamp(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "validated")
	data := "profile_digest=" + strings.Repeat("a", 64) + "\n" +
		"compiler_identity_digest=" + strings.Repeat("b", 64) + "\n" +
		"validation_scope=compiler\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRejectsUnsupportedCommand(t *testing.T) {
	if err := run([]string{"unknown"}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("run(unknown) error = %v", err)
	}
}
