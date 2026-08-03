package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeExecutor struct {
	nmOutput      string
	nmErr         error
	generateErr   error
	generated     bool
	compiler      string
	compilerArgs  []string
	compilerInput string
	genksyms      string
	genksymsArgs  []string
}

func (f *fakeExecutor) nm(_, _ string) ([]byte, error) {
	return []byte(f.nmOutput), f.nmErr
}

func (f *fakeExecutor) generate(compiler string, compilerArgs []string, compilerInput io.Reader, genksyms string, genksymsArgs []string, output io.Writer) error {
	f.generated = true
	f.compiler = compiler
	f.compilerArgs = append([]string(nil), compilerArgs...)
	f.genksyms = genksyms
	f.genksymsArgs = append([]string(nil), genksymsArgs...)
	if compilerInput != nil {
		input, err := io.ReadAll(compilerInput)
		if err != nil {
			return err
		}
		f.compilerInput = string(input)
	}
	if f.generateErr != nil {
		return f.generateErr
	}
	_, err := io.WriteString(output, "#SYMVER exported 0x12345678\n")
	return err
}

func TestRunPassesReferenceFileToGenksyms(t *testing.T) {
	cfg := testConfig(t, "c")
	cfg.linuxVersion = "6.12.96"
	cfg.reference = filepath.Join(t.TempDir(), "empty.symref")
	executor := &fakeExecutor{nmOutput: "00000000 D __export_symbol_exported\n"}
	if err := run(cfg, executor); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(executor.genksymsArgs, " "), "-r "+cfg.reference; got != want {
		t.Fatalf("genksyms args = %q, want %q", got, want)
	}
}

func TestRunOmitsReferenceFileWhenUnset(t *testing.T) {
	cfg := testConfig(t, "c")
	cfg.linuxVersion = "6.18.39"
	executor := &fakeExecutor{nmOutput: "00000000 D __export_symbol_exported\n"}
	if err := run(cfg, executor); err != nil {
		t.Fatal(err)
	}
	if len(executor.genksymsArgs) != 0 {
		t.Fatalf("genksyms args = %q, want none", executor.genksymsArgs)
	}
}

func testConfig(t *testing.T, mode string) config {
	t.Helper()
	return config{
		mode:         mode,
		nm:           "llvm-nm",
		object:       "kernel/example.o",
		compiler:     "clang",
		genksyms:     "genksyms",
		out:          filepath.Join(t.TempDir(), ".example.o.cmd"),
		linuxVersion: "6.18.39",
		compilerArgs: []string{"-E", "-D__GENKSYMS__", "example.c"},
	}
}

func TestRunSkipsPreprocessingWithoutExports(t *testing.T) {
	cfg := testConfig(t, "c")
	executor := &fakeExecutor{nmOutput: "00000000 T ordinary_symbol\n"}
	if err := run(cfg, executor); err != nil {
		t.Fatal(err)
	}
	if executor.generated {
		t.Fatal("preprocessor and genksyms ran without exported symbols")
	}
	output, err := os.ReadFile(cfg.out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(output), "\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunCPreprocessesExports(t *testing.T) {
	cfg := testConfig(t, "c")
	executor := &fakeExecutor{nmOutput: "00000000 D __export_symbol_exported\n"}
	if err := run(cfg, executor); err != nil {
		t.Fatal(err)
	}
	if !executor.generated {
		t.Fatal("preprocessor and genksyms did not run")
	}
	if executor.compilerInput != "" {
		t.Fatalf("compiler input = %q, want empty", executor.compilerInput)
	}
	if got, want := strings.Join(executor.compilerArgs, " "), "-E -D__GENKSYMS__ example.c"; got != want {
		t.Fatalf("compiler args = %q, want %q", got, want)
	}
	output, err := os.ReadFile(cfg.out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(output), "\n#SYMVER exported 0x12345678\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunAsmBuildsVersionedUpstreamInput(t *testing.T) {
	for _, tc := range []struct {
		version     string
		wantStringH bool
	}{
		{version: "6.12.96", wantStringH: false},
		{version: "6.18.39", wantStringH: true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			cfg := testConfig(t, "asm")
			cfg.linuxVersion = tc.version
			cfg.compilerArgs = []string{"-E", "-D__GENKSYMS__", "-xc", "-"}
			executor := &fakeExecutor{nmOutput: "00000000 D __export_symbol_from_asm\n"}
			if err := run(cfg, executor); err != nil {
				t.Fatal(err)
			}
			for _, required := range []string{
				"#include <linux/kernel.h>\n",
				"#include <asm/asm-prototypes.h>\n",
				"EXPORT_SYMBOL(from_asm);\n",
			} {
				if !strings.Contains(executor.compilerInput, required) {
					t.Errorf("compiler input %q does not contain %q", executor.compilerInput, required)
				}
			}
			if got := strings.Contains(executor.compilerInput, "#include <linux/string.h>\n"); got != tc.wantStringH {
				t.Errorf("linux/string.h present = %t, want %t", got, tc.wantStringH)
			}
		})
	}
}

func TestParseArgsValidatesModeAndCompilerArguments(t *testing.T) {
	base := []string{
		"-nm", "llvm-nm",
		"-object", "example.o",
		"-compiler", "clang",
		"-genksyms", "genksyms",
		"-out", ".example.o.cmd",
	}
	if _, err := parseArgs(append(append([]string{}, base...), "-mode", "invalid", "--", "-E")); err == nil {
		t.Fatal("invalid mode was accepted")
	}
	if _, err := parseArgs(base); err == nil {
		t.Fatal("missing compiler arguments were accepted")
	}
	if _, err := parseArgs(append(append([]string{}, base...), "-mode", "asm", "--", "-E")); err == nil {
		t.Fatal("asm mode without Linux version was accepted")
	}
}

func TestParseArgsAcceptsReferenceFile(t *testing.T) {
	cfg, err := parseArgs([]string{
		"-nm", "llvm-nm",
		"-object", "example.o",
		"-compiler", "clang",
		"-genksyms", "genksyms",
		"-reference", "empty.symref",
		"-out", ".example.o.cmd",
		"--", "-E", "example.c",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.reference, "empty.symref"; got != want {
		t.Fatalf("reference = %q, want %q", got, want)
	}
}

func TestExportedSymbolsMatchesUpstreamNmPattern(t *testing.T) {
	output := []byte(strings.Join([]string{
		"00000000 D __export_symbol_first",
		"00000001 T ordinary_symbol",
		"00000002 D __export_symbol_second",
		"__export_symbol_missing_leading_space",
	}, "\n"))
	got := exportedSymbols(output)
	if want := []string{"first", "second"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("symbols = %q, want %q", got, want)
	}
}
