package kconfig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeProbeTool(t *testing.T, path, version, counter string) {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo '` + version + `'
  exit 0
fi
printf x >> '` + counter + `'
for arg in "$@"; do
  if [ "$arg" = "-fnot-supported" ]; then
    exit 1
  fi
done
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func testRealToolProbe(t *testing.T, profile string) (*LinuxToolProbe, string) {
	return testRealToolProbeWithNames(t, profile, "clang", "ld.lld")
}

func testRealToolProbeWithNames(t *testing.T, profile, clangName, lldName string) (*LinuxToolProbe, string) {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	clang := filepath.Join(dir, clangName)
	lld := filepath.Join(dir, lldName)
	writeProbeTool(t, clang, "clang version 22.1.8", counter)
	writeProbeTool(t, lld, "LLD version 22.1.8", counter)
	target, err := LinuxTargetProfileByName(profile)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := NewLinuxToolProbe(LinuxToolProbeOptions{
		Profile: profile, Architecture: target.Arch, TargetTriple: target.TargetTriple,
		ClangPath: clang, LLDPath: lld, TempDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return probe, counter
}

func TestProbeToolModeIsExecutable(t *testing.T) {
	for _, test := range []struct {
		name string
		goos string
		mode os.FileMode
		want bool
	}{
		{name: "windows regular file", goos: "windows", mode: 0o666, want: true},
		{name: "windows directory", goos: "windows", mode: os.ModeDir | 0o777, want: false},
		{name: "linux executable", goos: "linux", mode: 0o755, want: true},
		{name: "linux regular file", goos: "linux", mode: 0o644, want: false},
		{name: "darwin executable", goos: "darwin", mode: 0o755, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := probeToolModeIsExecutable(test.goos, test.mode); got != test.want {
				t.Errorf("probeToolModeIsExecutable(%q, %v) = %v, want %v", test.goos, test.mode, got, test.want)
			}
		})
	}
}

func TestLinuxProbeShellWithToolsAcceptsWindowsSuffixedToolPaths(t *testing.T) {
	probe, _ := testRealToolProbeWithNames(t, "armv7", "clang.exe", "ld.lld.exe")
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		command string
		want    string
	}{
		{command: `{ command -v ` + probe.clangPath + `; } >/dev/null 2>&1 && echo "y" || echo "n"`, want: "y"},
		{command: `{ command -v ` + probe.lldPath + `; } >/dev/null 2>&1 && echo "y" || echo "n"`, want: "y"},
		{command: "/src/scripts/cc-version.sh " + probe.clangPath, want: "Clang 220108"},
		{command: "/src/scripts/as-version.sh " + probe.clangPath + " -fintegrated-as", want: "LLVM 0"},
		{command: "/src/scripts/ld-version.sh " + probe.lldPath, want: "LLD 220108"},
		{command: probe.clangPath + " --version", want: "clang version 22.1.8"},
	} {
		got, runErr := shell(context.Background(), test.command)
		if runErr != nil || got != test.want {
			t.Errorf("shell(%q) = %q, %v; want %q", test.command, got, runErr, test.want)
		}
	}
}

func TestLinuxToolProbeRunsAndCachesRealCompilerProbe(t *testing.T) {
	probe, counter := testRealToolProbe(t, "armv7")
	for i := 0; i < 2; i++ {
		supported, err := probe.SupportsOption(context.Background(), "cc_option", []string{"-fno-stack-protector"}, nil)
		if err != nil || !supported {
			t.Fatalf("SupportsOption() = %v, %v", supported, err)
		}
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "x" {
		t.Fatalf("tool executed %d times, want one cached execution", len(data))
	}
	if !strings.HasPrefix(probe.Identity(), "sha256-") {
		t.Fatalf("identity = %q", probe.Identity())
	}
}

func TestLinuxProbeShellWithToolsRejectsHostNativeMarchWithoutExecution(t *testing.T) {
	probe, counter := testRealToolProbe(t, "x86_64")
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	command := `{ clang -Werror -march=native -c -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`
	got, err := shell(context.Background(), command)
	if err != nil || got != "n" {
		t.Fatalf("shell(%q) = %q, %v; want n, nil", command, got, err)
	}
	data, readErr := os.ReadFile(counter)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(data) != 0 {
		t.Fatalf("host-native probe executed the compiler %d times, want zero", len(data))
	}
}

func TestLinuxToolProbeMeasuresSafeKbuildAssemblerSource(t *testing.T) {
	dir := t.TempDir()
	clang := filepath.Join(dir, "clang")
	lld := filepath.Join(dir, "ld.lld")
	clangScript := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo 'clang version 22.1.8'
  exit 0
fi
input=$(/bin/cat)
case "$input" in
  'mov r0,r0') ;;
  *) exit 1 ;;
esac
case " $* " in
  *' -Wa,--fatal-warnings '*) ;;
  *) exit 1 ;;
esac
case " $* " in
  *' /repository/include '*) exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(clang, []byte(clangScript), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProbeTool(t, lld, "LLD version 22.1.8", filepath.Join(dir, "count"))
	probe, err := NewLinuxToolProbe(LinuxToolProbeOptions{
		Profile: "armv7", Architecture: "arm", TargetTriple: "arm-linux-gnueabi",
		ClangPath: clang, LLDPath: lld, TempDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	supported, err := probe.SupportsKbuildSource(
		context.Background(),
		"assembler-with-cpp",
		"mov r0,r0",
		[]string{"-fintegrated-as", "-I", "/repository/include"},
	)
	if err != nil || !supported {
		t.Fatalf("SupportsKbuildSource() = %v, %v; want true", supported, err)
	}
	if _, err := probe.SupportsKbuildSource(
		context.Background(),
		"assembler-with-cpp",
		`.incbin "/etc/passwd"`,
		nil,
	); err == nil || !strings.Contains(err.Error(), "unsafe assembler") {
		t.Fatalf("unsafe Kbuild source error = %v, want assembler validation failure", err)
	}

	decoded, err := decodeKbuildPrintfB(`.cfi_startproc\n.cfi_endproc`)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != ".cfi_startproc\n.cfi_endproc\n" {
		t.Fatalf("decoded printf source = %q", decoded)
	}
	if err := validateKbuildAssemblerProbeSource(decoded); err != nil {
		t.Fatalf("valid CFI as-instr source rejected: %v", err)
	}
}

func TestLinuxToolProbeReturnsUnsupportedExit(t *testing.T) {
	probe, _ := testRealToolProbe(t, "x86_64")
	supported, err := probe.SupportsOption(context.Background(), "cc_option", []string{"-fnot-supported"}, nil)
	if err != nil || supported {
		t.Fatalf("SupportsOption() = %v, %v; want false, nil", supported, err)
	}
}

func TestLinuxToolProbeAllowsARMAPCSMachineFlag(t *testing.T) {
	probe, _ := testRealToolProbe(t, "armv7")
	supported, err := probe.SupportsOption(context.Background(), "cc_option", []string{"-mapcs"}, nil)
	if err != nil || !supported {
		t.Fatalf("SupportsOption(-mapcs) = %v, %v; want true, nil", supported, err)
	}
}

func TestLinuxToolProbeRejectsUnpinnedVersion(t *testing.T) {
	dir := t.TempDir()
	clang := filepath.Join(dir, "clang")
	lld := filepath.Join(dir, "ld.lld")
	writeProbeTool(t, clang, "clang version 21.1.8", filepath.Join(dir, "count"))
	writeProbeTool(t, lld, "LLD 22.1.8", filepath.Join(dir, "count"))
	_, err := NewLinuxToolProbe(LinuxToolProbeOptions{
		Profile: "x86_64", Architecture: "x86", TargetTriple: "x86_64-linux-gnu",
		ClangPath: clang, LLDPath: lld,
	})
	if err == nil || !strings.Contains(err.Error(), "want pinned LLVM") {
		t.Fatalf("NewLinuxToolProbe() error = %v", err)
	}
}

func TestLinuxToolProbeRunsAssemblerProbe(t *testing.T) {
	probe, counter := testRealToolProbe(t, "aarch64")
	supported, err := probe.SupportsOption(context.Background(), "as_option", []string{"-Wa,--fatal-warnings"}, []string{"-D__ASSEMBLY__"})
	if err != nil || !supported {
		t.Fatalf("SupportsOption(as_option) = %v, %v", supported, err)
	}
	if data, readErr := os.ReadFile(counter); readErr != nil || string(data) != "x" {
		t.Fatalf("assembler probe counter = %q, %v", data, readErr)
	}
}

func TestLinuxProbeShellWithToolsCompilesAllowlistedSource(t *testing.T) {
	probe, counter := testRealToolProbe(t, "aarch64")
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	command := `{ printf "%b\n" ".arch_extension lse" | clang -fintegrated-as -Wa,--fatal-warnings -c -x assembler-with-cpp -o /dev/null -; } >/dev/null 2>&1 && echo "y" || echo "n"`
	got, err := shell(context.Background(), command)
	if err != nil || got != "y" {
		t.Fatalf("shell() = %q, %v", got, err)
	}
	if data, readErr := os.ReadFile(counter); readErr != nil || string(data) != "x" {
		t.Fatalf("source probe counter = %q, %v", data, readErr)
	}
}

func TestLinuxProbeShellWithToolsDecodesAssemblerPrintfSource(t *testing.T) {
	dir := t.TempDir()
	clang := filepath.Join(dir, "clang")
	lld := filepath.Join(dir, "ld.lld")
	captured := filepath.Join(dir, "source")
	clangScript := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo 'clang version 22.1.8'
  exit 0
fi
/bin/cat > '` + captured + `'
`
	if err := os.WriteFile(clang, []byte(clangScript), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProbeTool(t, lld, "LLD version 22.1.8", filepath.Join(dir, "count"))
	probe, err := NewLinuxToolProbe(LinuxToolProbeOptions{
		Profile: "x86_64", Architecture: "x86", TargetTriple: "x86_64-linux-gnu",
		ClangPath: clang, LLDPath: lld, TempDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "printf escapes",
			source: `1:\n.inst 0\n.rept . - 1b\n\nnop\n.endr\n`,
			want:   "1:\n.inst 0\n.rept . - 1b\n\nnop\n.endr\n\n",
		},
		{
			name:   "double quoted dollar",
			source: `vpclmulqdq \$0x10,%ymm0,%ymm1,%ymm2`,
			want:   "vpclmulqdq $0x10,%ymm0,%ymm1,%ymm2\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := `{ printf "%b\n" "` + test.source + `" | clang -fintegrated-as -Wa,--fatal-warnings -c -x assembler-with-cpp -o /dev/null -; } >/dev/null 2>&1 && echo "y" || echo "n"`
			got, err := shell(context.Background(), command)
			if err != nil || got != "y" {
				t.Fatalf("shell() = %q, %v", got, err)
			}
			data, err := os.ReadFile(captured)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != test.want {
				t.Fatalf("assembler probe source = %q, want %q", data, test.want)
			}
		})
	}
}

func TestLinuxToolProbeFailsClosedBeforeExecution(t *testing.T) {
	probe, _ := testRealToolProbe(t, "aarch64")
	for _, candidate := range [][]string{
		{"@attacker.rsp"},
		{"/tmp/input.c"},
		{"-o", "/tmp/owned"},
		{"-DOK=1\n-fplugin=bad"},
		{"-fplugin=/tmp/evil.so"},
		{"-mllvm", "-load=/tmp/evil.so"},
		{"--script=/tmp/evil.ld"},
		{"--plugin=/tmp/evil.so"},
		{"-Map=/tmp/owned"},
		{"-L/tmp/evil"},
	} {
		if _, err := probe.SupportsOption(context.Background(), "cc_option", candidate, nil); err == nil {
			t.Fatalf("SupportsOption(%q) unexpectedly succeeded", candidate)
		}
	}
}

func TestSanitizeLinkerProbeContextAcceptsCanonicalOperandOptions(t *testing.T) {
	got, err := sanitizeProbeContext("ld_option", []string{
		"-m", "elf_x86_64",
		"-z", "noexecstack",
		"--no-ld-generated-unwind-info",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-m", "elf_x86_64",
		"-z", "noexecstack",
		"--no-ld-generated-unwind-info",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("sanitizeProbeContext() = %#v, want %#v", got, want)
	}
	for _, context := range [][]string{
		{"-m", "../../evil"},
		{"-z"},
	} {
		if _, err := sanitizeProbeContext("ld_option", context); err == nil {
			t.Errorf("sanitizeProbeContext(%#v) unexpectedly succeeded", context)
		}
	}
}

func TestLinuxToolProbeTimeoutAndOutputCap(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "timeout", body: `/bin/sleep 2`, want: "timed out"},
		{name: "output", body: `i=0; while [ "$i" -lt 200 ]; do printf x; i=$((i + 1)); done`, want: "output exceeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			clang := filepath.Join(dir, "clang")
			lld := filepath.Join(dir, "ld.lld")
			tool := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'clang version 22.1.8'; exit 0; fi
` + test.body + `
`
			if err := os.WriteFile(clang, []byte(tool), 0o755); err != nil {
				t.Fatal(err)
			}
			lldTool := strings.Replace(tool, "clang version", "LLD version", 1)
			if err := os.WriteFile(lld, []byte(lldTool), 0o755); err != nil {
				t.Fatal(err)
			}
			probe, err := NewLinuxToolProbe(LinuxToolProbeOptions{
				Profile: "x86_64", Architecture: "x86", TargetTriple: "x86_64-linux-gnu",
				ClangPath: clang, LLDPath: lld, TempDir: dir, Timeout: 50 * time.Millisecond, OutputLimit: 32,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = probe.SupportsOption(context.Background(), "cc_option", []string{"-fno-stack-protector"}, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("SupportsOption() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLinuxProbeShellWithToolsUsesMeasuredVersions(t *testing.T) {
	probe, _ := testRealToolProbe(t, "x86_64")
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string]string{
		"/src/scripts/cc-version.sh clang":  "Clang 220108",
		"/src/scripts/ld-version.sh ld.lld": "LLD 220108",
		"clang --version":                   "clang version 22.1.8",
	} {
		got, runErr := shell(context.Background(), command)
		if runErr != nil || got != want {
			t.Fatalf("shell(%q) = %q, %v; want %q", command, got, runErr, want)
		}
	}
}

func TestParseLinuxSourceProbeAcceptsKconfigCompilerVariables(t *testing.T) {
	for _, compiler := range []string{"$CC", "$(CC)", "/pinned/bin/clang"} {
		command := "echo 'int foo(void) { return 0; }' | " + compiler + " $(CLANG_FLAGS) -x c - -c -o /dev/null -Werror"
		source, candidate, err := parseLinuxSourceProbe(command)
		if err != nil {
			t.Fatalf("parseLinuxSourceProbe(%q): %v", command, err)
		}
		if source != "int foo(void) { return 0; }" {
			t.Fatalf("source = %q", source)
		}
		if got, want := strings.Join(candidate, " "), "-fintegrated-as -Werror"; got != want {
			t.Fatalf("candidate = %q, want %q", got, want)
		}
	}
}

func TestParseLinuxSourceProbeUnquotesVPCLMULImmediate(t *testing.T) {
	command := `printf "%b\n" "vpclmulqdq \$0x10,%ymm0,%ymm1,%ymm2" | clang -fintegrated-as -Wa,--fatal-warnings -c -x assembler-with-cpp -o /dev/null -`
	source, candidate, err := parseLinuxSourceProbe(command)
	if err != nil {
		t.Fatal(err)
	}
	if want := `vpclmulqdq $0x10,%ymm0,%ymm1,%ymm2`; source != want {
		t.Fatalf("source = %q, want %q", source, want)
	}
	if got, want := strings.Join(candidate, " "), "-fintegrated-as -Wa,--fatal-warnings"; got != want {
		t.Fatalf("candidate = %q, want %q", got, want)
	}
}

func TestKnownRELRProbeAcceptsPinnedToolPaths(t *testing.T) {
	command := `env "CC=/pinned/bin/clang" "LD=/pinned/bin/ld.lld" "NM=llvm-nm" "OBJCOPY=llvm-objcopy" /src/scripts/tools-support-relr.sh`
	if !isKnownRELRProbe(command) {
		t.Fatalf("isKnownRELRProbe(%q) = false", command)
	}
}
