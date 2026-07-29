package ccprofile

import (
	"strings"
	"testing"
)

func testSentinels() CompileSentinels {
	return CompileSentinels{
		Source:      "__LINUX_BZL_SOURCE__",
		Output:      "__LINUX_BZL_OUTPUT__",
		KbuildFlags: "__LINUX_BZL_KBUILD_FLAGS__",
	}
}

func TestExtractMutableCompileArgv(t *testing.T) {
	sentinels := testSentinels()
	got, err := ExtractMutableCompileArgv([]string{
		"--target=x86_64-unknown-linux-gnu",
		"-Wall",
		"-frandom-seed=" + sentinels.Output,
		"@toolchain.params",
		sentinels.KbuildFlags,
		"-c",
		sentinels.Source,
		"-o",
		sentinels.Output,
	}, sentinels)
	if err != nil {
		t.Fatalf("ExtractMutableCompileArgv() failed: %v", err)
	}
	want := []string{
		"--target=x86_64-unknown-linux-gnu",
		"-Wall",
		"@toolchain.params",
		sentinels.KbuildFlags,
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("mutable argv = %#v, want %#v", got, want)
	}
}

func TestExtractMutableCompileArgvAcceptsJoinedOutput(t *testing.T) {
	sentinels := testSentinels()
	got, err := ExtractMutableCompileArgv([]string{
		"-c",
		sentinels.Source,
		"-o" + sentinels.Output,
		sentinels.KbuildFlags,
	}, sentinels)
	if err != nil {
		t.Fatalf("ExtractMutableCompileArgv() failed: %v", err)
	}
	if len(got) != 1 || got[0] != sentinels.KbuildFlags {
		t.Fatalf("mutable argv = %#v", got)
	}
}

func TestExtractMutableCompileArgvRejectsAmbiguousSkeleton(t *testing.T) {
	sentinels := testSentinels()
	valid := []string{
		"-c",
		sentinels.Source,
		"-o",
		sentinels.Output,
		sentinels.KbuildFlags,
	}
	tests := map[string][]string{
		"embedded source": {
			"-c",
			"-include=" + sentinels.Source,
			sentinels.Source,
			"-o",
			sentinels.Output,
			sentinels.KbuildFlags,
		},
		"missing output": {
			"-c",
			sentinels.Source,
			sentinels.KbuildFlags,
		},
		"preprocess mode": append([]string{"-E"}, valid...),
		"unknown output binding": append(
			[]string{"-fdebug-prefix-map=" + sentinels.Output + "=."},
			valid...,
		),
	}
	for name, argv := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ExtractMutableCompileArgv(argv, sentinels); err == nil {
				t.Fatalf("ExtractMutableCompileArgv(%#v) succeeded", argv)
			}
		})
	}
}

func TestParseBuiltinMacrosAndCompilerVersion(t *testing.T) {
	macros, err := ParseBuiltinMacros([]byte(strings.Join([]string{
		"#define __GNUC__ 4",
		"#define __GNUC_MINOR__ 2",
		"#define __GNUC_PATCHLEVEL__ 1",
		"#define __clang_major__ 22",
		"#define __clang_minor__ 1",
		"#define __clang_patchlevel__ 8",
		"#define __has_feature(x) __has_builtin(x)",
		"#define EMPTY",
		"",
	}, "\n")))
	if err != nil {
		t.Fatalf("ParseBuiltinMacros() failed: %v", err)
	}
	name, version, err := compilerVersion(macros)
	if err != nil {
		t.Fatalf("compilerVersion() failed: %v", err)
	}
	if name != "Clang" || version != 220108 {
		t.Fatalf("compiler identity = %s %d", name, version)
	}
	if value, ok := macros["EMPTY"]; !ok || value != "" {
		t.Fatalf("EMPTY macro = %q, present %t", value, ok)
	}
	if _, ok := macros["__has_feature(x)"]; ok {
		t.Fatal("function-like macro was retained")
	}
}

func TestValidateProfileIdentityUsesExpectedMacroSubset(t *testing.T) {
	profile := validProfile()
	actual := CompilerIdentity{
		Schema:           CompilerIdentitySchema,
		Architecture:     profile.Architecture,
		DriverContract:   profile.DriverContract,
		AnalysisIdentity: profile.AnalysisIdentity,
		CCName:           profile.KconfigIdentity.CCName,
		CCVersion:        profile.KconfigIdentity.CCVersion,
		CCVersionText:    profile.KconfigIdentity.CCVersionText,
		BuiltinMacros: map[string]string{
			"__SIZEOF_INT128__": "16",
			"UNPROFILED_MACRO":  "allowed",
		},
	}
	if err := ValidateProfileIdentity(profile, actual); err != nil {
		t.Fatalf("ValidateProfileIdentity() failed: %v", err)
	}
	actual.BuiltinMacros["__SIZEOF_INT128__"] = "8"
	if err := ValidateProfileIdentity(profile, actual); err == nil ||
		!strings.Contains(err.Error(), "__SIZEOF_INT128__") {
		t.Fatalf("ValidateProfileIdentity(mismatch) error = %v", err)
	}
}
