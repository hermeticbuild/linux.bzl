package kconfig

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeProbeTool(t *testing.T, path, version, counter string) {
	t.Helper()
	macroIdentity := "unknown"
	if matches := semanticVersionPattern.FindAllStringSubmatch(version, -1); len(matches) != 0 {
		parts := matches[len(matches)-1]
		patch := parts[3]
		if patch == "" {
			patch = "0"
		}
		if strings.Contains(strings.ToLower(version), "clang") {
			macroIdentity = "Clang " + parts[1] + " " + parts[2] + " " + patch
		}
	}
	script := `#!/bin/sh
case " $* " in
  *' --version '*) echo '` + version + `'; exit 0 ;;
  *' -E -P -x c - '*) echo '` + macroIdentity + `'; exit 0 ;;
  *' -### '*) echo '"clang" "-cc1as"' >&2; exit 0 ;;
esac
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

func addAuxiliaryProbeTools(t *testing.T, dir, family string, opts *LinuxToolProbeOptions) {
	t.Helper()
	outputs := map[string]string{}
	switch family {
	case "llvm":
		outputs = map[string]string{
			"llvm-ar":      "LLVM archiver version 22.1.8",
			"llvm-nm":      "LLVM nm version 22.1.8",
			"llvm-objcopy": "llvm-objcopy version 22.1.8",
		}
	case "gnu":
		outputs = map[string]string{
			"ar":      "GNU ar (GNU Binutils) 2.44",
			"nm":      "GNU nm (GNU Binutils) 2.44",
			"objcopy": "GNU objcopy (GNU Binutils) 2.44",
		}
	default:
		t.Fatalf("unsupported auxiliary test tool family %q", family)
	}
	for name, output := range outputs {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+output+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.HasSuffix(name, "objcopy"):
			opts.ObjcopyPath = path
		case strings.HasSuffix(name, "nm"):
			opts.NMPath = path
		default:
			opts.ArchiverPath = path
		}
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
	opts := LinuxToolProbeOptions{
		Profile: profile, Architecture: target.Arch, TargetTriple: target.TargetTriple,
		CompilerPath: clang, LinkerPath: lld, TempDir: dir,
	}
	addAuxiliaryProbeTools(t, dir, "llvm", &opts)
	probe, err := NewLinuxToolProbe(opts)
	if err != nil {
		t.Fatal(err)
	}
	return probe, counter
}

func testGCCProbe(t *testing.T) (*LinuxToolProbe, string) {
	return testGCCProbeWithName(t, "gcc")
}

func testGCCProbeWithName(t *testing.T, compilerName string) (*LinuxToolProbe, string) {
	t.Helper()
	dir := t.TempDir()
	invocations := filepath.Join(dir, "invocations")
	gcc := filepath.Join(dir, compilerName)
	ld := filepath.Join(dir, "ld")
	pluginDir := filepath.Join(dir, "plugin")
	if err := os.MkdirAll(filepath.Join(pluginDir, "include"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "include", "plugin-version.h"), []byte("/* test */\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gccScript := `#!/bin/sh
case " $* " in
  *' --version '*) echo 'gcc (GCC) 15.2.0'; exit 0 ;;
  *' -E -P -x c - '*) echo 'GCC 15 2 0'; exit 0 ;;
  *' -print-file-name=plugin '*) echo '` + pluginDir + `'; exit 0 ;;
esac
printf '%s\n' "$*" >> '` + invocations + `'
case " $* " in
	*' -Wa,--version '*) echo 'GNU assembler (GNU Binutils) 2.44'; exit 0 ;;
  *' -fnot-supported '*) exit 1 ;;
esac
out=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; out="$1"; fi
  shift
done
[ -z "$out" ] || [ "$out" = "-" ] || : > "$out"
exit 0
`
	ldScript := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo 'GNU ld (GNU Binutils) 2.44'
  exit 0
fi
printf 'ld %s\n' "$*" >> '` + invocations + `'
exit 0
`
	if err := os.WriteFile(gcc, []byte(gccScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ld, []byte(ldScript), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := LinuxToolProbeOptions{
		Profile: "x86_64", Architecture: "x86", TargetTriple: "x86_64-linux-gnu",
		CompilerPath: gcc, LinkerPath: ld, TempDir: dir,
	}
	addAuxiliaryProbeTools(t, dir, "gnu", &opts)
	probe, err := NewLinuxToolProbe(opts)
	if err != nil {
		t.Fatal(err)
	}
	return probe, invocations
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

func TestToolIdentityIncludesAssembler(t *testing.T) {
	dir := t.TempDir()
	compiler := filepath.Join(dir, "compiler")
	assembler := filepath.Join(dir, "assembler")
	linker := filepath.Join(dir, "linker")
	for path, contents := range map[string]string{
		compiler:  "compiler",
		assembler: "assembler-one",
		linker:    "linker",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	first, err := toolIdentity(compiler, assembler, linker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assembler, []byte("assembler-two"), 0o755); err != nil {
		t.Fatal(err)
	}
	second, err := toolIdentity(compiler, assembler, linker)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("tool identity did not change with assembler: %q", first)
	}
}

func TestToolIdentityIncludesVersionOutput(t *testing.T) {
	dir := t.TempDir()
	compiler := filepath.Join(dir, "compiler")
	assembler := filepath.Join(dir, "assembler")
	linker := filepath.Join(dir, "linker")
	for _, path := range []string{compiler, assembler, linker} {
		if err := os.WriteFile(path, []byte("stable wrapper"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	first, err := toolIdentityWithVersionLines(
		[]string{"gcc (GCC) 14.3.0", "GNU assembler 2.44", "GNU ld 2.44"},
		compiler,
		assembler,
		linker,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := toolIdentityWithVersionLines(
		[]string{"gcc (GCC) 15.2.0", "GNU assembler 2.44", "GNU ld 2.44"},
		compiler,
		assembler,
		linker,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("tool identity did not change with compiler version: %q", first)
	}
}

func TestConfiguredToolIdentityIncludesCompilerPrefix(t *testing.T) {
	profile, err := LinuxTargetProfileByName("x86_64")
	if err != nil {
		t.Fatal(err)
	}
	first := extendToolIdentity("sha256-base", profile, []string{"-DTOOLCHAIN=one"}, nil)
	second := extendToolIdentity("sha256-base", profile, []string{"-DTOOLCHAIN=two"}, nil)
	if first == second {
		t.Fatalf("configured tool identities are equal: %q", first)
	}
	linked := extendToolIdentity("sha256-base", profile, []string{"-DTOOLCHAIN=one"}, []string{"-fuse-ld=lld"})
	if first == linked {
		t.Fatalf("linker-driver prefix did not change tool identity: %q", first)
	}
	policyDisabled := extendToolIdentity("sha256-base", profile, []string{"-DTOOLCHAIN=one"}, nil, false, false)
	if first == policyDisabled {
		t.Fatalf("measured probe policy did not change tool identity: %q", first)
	}
}

func TestValidateCompilerPrefixArgsRejectsControlledOutputs(t *testing.T) {
	for _, args := range [][]string{
		{"-c"},
		{"-o", "stolen.o"},
		{"-MF=stolen.d"},
		{"@response.params"},
	} {
		if err := validateCompilerPrefixArgs(args); err == nil {
			t.Fatalf("validateCompilerPrefixArgs(%q) succeeded", args)
		}
	}
	if err := validateCompilerPrefixArgs([]string{"-nostdinc", "-B/toolchain/bin", "--target=x86_64-linux-gnu"}); err != nil {
		t.Fatalf("validateCompilerPrefixArgs() rejected safe prefix: %v", err)
	}
}

func TestValidateLinkerDriverPrefixArgs(t *testing.T) {
	for _, args := range [][]string{
		{"-o", "stolen"},
		{"input.o"},
		{"@response.params"},
	} {
		if err := validateLinkerDriverPrefixArgs(args); err == nil {
			t.Fatalf("validateLinkerDriverPrefixArgs(%q) succeeded", args)
		}
	}
	if err := validateLinkerDriverPrefixArgs([]string{"-B/toolchain/bin", "-L/toolchain/lib", "-fuse-ld=lld"}); err != nil {
		t.Fatalf("validateLinkerDriverPrefixArgs() failed: %v", err)
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
		{command: `{ command -v ` + probe.compilerPath + `; } >/dev/null 2>&1 && echo "y" || echo "n"`, want: "y"},
		{command: `{ command -v ` + probe.linkerPath + `; } >/dev/null 2>&1 && echo "y" || echo "n"`, want: "y"},
		{command: "/src/scripts/cc-version.sh " + probe.compilerPath, want: "Clang 220108"},
		{command: "/src/scripts/as-version.sh " + probe.compilerPath + " -fintegrated-as", want: "LLVM 0"},
		{command: "/src/scripts/ld-version.sh " + probe.linkerPath, want: "LLD 220108"},
		{command: probe.compilerPath + " --version", want: "clang version 22.1.8"},
	} {
		got, runErr := shell(context.Background(), test.command)
		if runErr != nil || got != test.want {
			t.Errorf("shell(%q) = %q, %v; want %q", test.command, got, runErr, test.want)
		}
	}
}

func TestLinuxToolProbeDetectsClangExternalGNUAssembler(t *testing.T) {
	dir := t.TempDir()
	clang := filepath.Join(dir, "clang")
	lld := filepath.Join(dir, "ld.lld")
	invocations := filepath.Join(dir, "invocations")
	clangScript := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'clang version 22.1.8'; exit 0; fi
case " $* " in
	*' -E -P -x c - '*) echo 'Clang 22 1 8'; exit 0 ;;
  *' -### '*) echo '"/toolchain/bin/as" "--64"' >&2; exit 0 ;;
  *' -Wa,--version '*) echo 'GNU assembler (GNU Binutils) 2.44'; exit 0 ;;
esac
	printf '%s\n' "$*" >> '` + invocations + `'
	echo 'movq %gs:40, %rax'
exit 0
`
	if err := os.WriteFile(clang, []byte(clangScript), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProbeTool(t, lld, "LLD version 22.1.8", filepath.Join(dir, "counter"))
	opts := LinuxToolProbeOptions{
		Profile: "x86_64", Architecture: "x86", TargetTriple: "x86_64-linux-gnu",
		CompilerPath: clang, LinkerPath: lld, TempDir: dir,
	}
	addAuxiliaryProbeTools(t, dir, "gnu", &opts)
	probe, err := NewLinuxToolProbe(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := probe.ClangFlags(), "-fno-integrated-as"; got != want {
		t.Fatalf("ClangFlags() = %q, want %q", got, want)
	}
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	command := "/src/scripts/as-version.sh " + clang + " -fno-integrated-as"
	if got, err := shell(context.Background(), command); err != nil || got != "GNU 24400" {
		t.Fatalf("shell(%q) = %q, %v; want GNU 24400", command, got, err)
	}
	assertExternalAssemblerInvocation := func(command string) {
		t.Helper()
		data, err := os.ReadFile(invocations)
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(data))
		if !slices.Contains(fields, "-fno-integrated-as") {
			t.Fatalf("compiler invocation for %s = %q, missing -fno-integrated-as", command, data)
		}
		if slices.Contains(fields, "-fintegrated-as") {
			t.Fatalf("compiler invocation for %s = %q, unexpectedly selected the integrated assembler", command, data)
		}
	}
	resetInvocations := func() {
		t.Helper()
		if err := os.WriteFile(invocations, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		name       string
		clangFlags string
	}{
		{name: "variable", clangFlags: "$(CLANG_FLAGS)"},
		{name: "expanded", clangFlags: "-fno-integrated-as"},
	} {
		resetInvocations()
		command := `{ ` + clang + ` -Werror ` + test.clangFlags + ` -fexternal-assembler-` + test.name + `-test -c -x c /dev/null -o .tmp.o; } >/dev/null 2>&1 && echo "y" || echo "n"`
		if got, err := shell(context.Background(), command); err != nil || got != "y" {
			t.Fatalf("shell(%q) = %q, %v; want y", command, got, err)
		}
		assertExternalAssemblerInvocation("cc-option " + test.name)
	}

	resetInvocations()
	command = `{ printf "%b\n" ".arch_extension lse" | ` + clang + ` $(CLANG_FLAGS) -Wa,--fatal-warnings -c -x assembler-with-cpp -o /dev/null -; } >/dev/null 2>&1 && echo "y" || echo "n"`
	if got, err := shell(context.Background(), command); err != nil || got != "y" {
		t.Fatalf("shell(%q) = %q, %v; want y", command, got, err)
	}
	assertExternalAssemblerInvocation("source probe")

	resetInvocations()
	command = `{ /src/scripts/gcc-x86_64-has-stack-protector.sh ` + clang + ` -fno-integrated-as; } >/dev/null 2>&1 && echo "y" || echo "n"`
	if got, err := shell(context.Background(), command); err != nil || got != "y" {
		t.Fatalf("shell(%q) = %q, %v; want y", command, got, err)
	}
	assertExternalAssemblerInvocation("x86 stack-protector probe")
	if _, err := probe.SupportsX86StackProtector(context.Background(), 64, []string{"-fintegrated-as"}); err == nil || !strings.Contains(err.Error(), "invalid x86 stack-protector script argument") {
		t.Fatalf("SupportsX86StackProtector() mismatched assembler error = %v", err)
	}
	if _, err := probe.SupportsX86StackProtector(context.Background(), 64, nil); err == nil || !strings.Contains(err.Error(), "invalid x86 stack-protector script arguments") {
		t.Fatalf("SupportsX86StackProtector() missing assembler argument error = %v", err)
	}
	if _, err := probe.SupportsX86StackProtector(context.Background(), 64, []string{"-fno-integrated-as", "-m64"}); err == nil || !strings.Contains(err.Error(), "invalid x86 stack-protector script argument") {
		t.Fatalf("SupportsX86StackProtector() extra argument error = %v", err)
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

func TestLinuxToolProbeMeasuresGCCGNUIdentity(t *testing.T) {
	probe, invocations := testGCCProbe(t)
	if got, want := probe.CompilerFamily(), "gcc"; got != want {
		t.Fatalf("CompilerFamily() = %q, want %q", got, want)
	}
	if got, want := probe.CompilerVersionText(), "gcc (GCC) 15.2.0"; got != want {
		t.Fatalf("CompilerVersionText() = %q, want %q", got, want)
	}
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string]string{
		"/src/scripts/cc-version.sh " + probe.compilerPath: "GCC 150200",
		"/src/scripts/as-version.sh " + probe.compilerPath: "GNU 24400",
		"/src/scripts/ld-version.sh " + probe.linkerPath:   "BFD 24400",
		probe.compilerPath + " --version":                  "gcc (GCC) 15.2.0",
	} {
		got, runErr := shell(context.Background(), command)
		if runErr != nil || got != want {
			t.Fatalf("shell(%q) = %q, %v; want %q", command, got, runErr, want)
		}
	}
	data, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "-Wa,--version -c -x assembler-with-cpp "+os.DevNull+" -o "+os.DevNull) {
		t.Fatalf("assembler was not identified through the compiler driver: %q", data)
	}
}

func TestLinuxToolProbeIdentifiesConfiguredFrontendFromMacros(t *testing.T) {
	dir := t.TempDir()
	compiler := filepath.Join(dir, "compiler-wrapper")
	linker := filepath.Join(dir, "ld.lld")
	invocations := filepath.Join(dir, "compiler-invocations")
	compilerScript := `#!/bin/sh
printf '%s\n' "$*" >> '` + invocations + `'
case " $* " in
  ' --version '*) echo 'opaque wrapper version 1.0'; exit 0 ;;
  *' --frontend=clang --version '*) echo 'clang version 22.1.8'; exit 0 ;;
  *' --frontend=clang -E -P -x c - '*) echo 'Clang 22 1 9'; exit 0 ;;
  *' --frontend=clang -### '*) echo '"clang" "-cc1as"' >&2; exit 0 ;;
esac
exit 1
`
	if err := os.WriteFile(compiler, []byte(compilerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProbeTool(t, linker, "LLD version 22.1.8", filepath.Join(dir, "count"))
	opts := LinuxToolProbeOptions{
		Profile: "x86_64", Architecture: "x86", TargetTriple: "x86_64-linux-gnu",
		CompilerPath: compiler, LinkerPath: linker, CompilerArgs: []string{"--frontend=clang"}, TempDir: dir,
	}
	addAuxiliaryProbeTools(t, dir, "llvm", &opts)
	probe, err := NewLinuxToolProbe(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := probe.CompilerFamily(), "clang"; got != want {
		t.Fatalf("CompilerFamily() = %q, want %q", got, want)
	}
	if got, want := probe.compilerCode, 220109; got != want {
		t.Fatalf("compilerCode = %d, want %d", got, want)
	}
	if got, want := probe.CompilerVersionText(), "clang version 22.1.8"; got != want {
		t.Fatalf("CompilerVersionText() = %q, want %q", got, want)
	}
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	if got, runErr := shell(context.Background(), "/src/scripts/cc-version.sh "+compiler); runErr != nil || got != "Clang 220109" {
		t.Fatalf("configured cc-version = %q, %v; want Clang 220109", got, runErr)
	}
	data, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--frontend=clang --version", "--frontend=clang -E -P -x c -"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("configured compiler invocations %q do not contain %q", data, want)
		}
	}
}

func TestLinuxToolProbeLegacyClangPrefixAddsNonHostTarget(t *testing.T) {
	for _, name := range []string{"x86_64", "aarch64", "armv7", "riscv64", "ppc64le"} {
		profile, err := LinuxTargetProfileByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if linuxProbeProfileMatchesHost(profile) {
			continue
		}
		probe := &LinuxToolProbe{profile: profile, compilerFamily: "clang"}
		got := probe.configuredCompilerArgs()
		want := []string{"--target=" + profile.TargetTriple}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("configuredCompilerArgs() = %q, want %q", got, want)
		}
		probe.compilerArgs = []string{"-DTOOLCHAIN_CONFIGURED=1"}
		if got := probe.configuredCompilerArgs(); len(got) != 1 || got[0] != "-DTOOLCHAIN_CONFIGURED=1" {
			t.Fatalf("configured compiler prefix was not authoritative: %q", got)
		}
		return
	}
	t.Fatal("no non-host Linux target profile found")
}

func TestLinuxProbeShellWithToolsMeasuresCCCanLink(t *testing.T) {
	probe, invocations := testGCCProbe(t)
	probe.compilerArgs = []string{"-DTOOLCHAIN_PREFIX=1"}
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	command := `{ /src/scripts/cc-can-link.sh ` + probe.compilerPath + ` -m64 -static; } >/dev/null 2>&1 && echo "y" || echo "n"`
	got, err := shell(context.Background(), command)
	if err != nil || got != "y" {
		t.Fatalf("shell(%q) = %q, %v; want y, nil", command, got, err)
	}
	data, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-DTOOLCHAIN_PREFIX=1", "-m64", "-static", "-Werror", "-Wl,--fatal-warnings"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("cc-can-link invocation %q does not contain %q", data, want)
		}
	}
	unsafe := `{ /src/scripts/cc-can-link.sh ` + probe.compilerPath + ` -o /tmp/escape; } >/dev/null 2>&1 && echo "y" || echo "n"`
	if _, err := shell(context.Background(), unsafe); err == nil || !strings.Contains(err.Error(), "controlled") {
		t.Fatalf("unsafe cc-can-link error = %v", err)
	}
	toolSelection := `{ /src/scripts/cc-can-link.sh ` + probe.compilerPath + ` -B/undeclared; } >/dev/null 2>&1 && echo "y" || echo "n"`
	if _, err := shell(context.Background(), toolSelection); err == nil || !strings.Contains(err.Error(), "tool-selection") {
		t.Fatalf("tool-selecting cc-can-link error = %v", err)
	}
}

func TestLinuxProbeShellWithToolsReproducesCCOptionBitPreprocessing(t *testing.T) {
	probe, invocations := testGCCProbe(t)
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	command := `{ gcc -Werror -m64 -E -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`
	got, err := shell(context.Background(), command)
	if err != nil || got != "y" {
		t.Fatalf("shell(%q) = %q, %v; want y, nil", command, got, err)
	}
	data, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	invocation := string(data)
	lines := strings.Split(strings.TrimSpace(invocation), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(" "+last+" ", " -E ") || strings.Contains(" "+last+" ", " -c ") {
		t.Fatalf("cc-option-bit invocation = %q, want preprocessing without assembly", invocation)
	}
}

func TestLinuxProbeShellWithToolsMeasuresGCCPluginHeader(t *testing.T) {
	probe, _ := testGCCProbe(t)
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	pluginDir, err := shell(context.Background(), probe.compilerPath+" -print-file-name=plugin")
	if err != nil {
		t.Fatal(err)
	}
	header := filepath.Join(pluginDir, "include", "plugin-version.h")
	command := `{ test -e ` + header + `; } >/dev/null 2>&1 && echo "y" || echo "n"`
	got, err := shell(context.Background(), command)
	if err != nil || got != "y" {
		t.Fatalf("shell(%q) = %q, %v; want y, nil", command, got, err)
	}
	wrong := `{ test -e ` + filepath.Join(t.TempDir(), "include", "plugin-version.h") + `; } >/dev/null 2>&1 && echo "y" || echo "n"`
	if _, err := shell(context.Background(), wrong); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched plugin header error = %v", err)
	}
}

func TestLinuxProbeShellWithToolsHonorsDisabledLinkAndPluginPolicy(t *testing.T) {
	probe, invocations := testGCCProbe(t)
	probe.disableCCCanLink = true
	probe.disableGCCPlugins = true
	if err := os.WriteFile(invocations, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	linkCommand := `{ /src/scripts/cc-can-link.sh ` + probe.compilerPath + ` -m64; } >/dev/null 2>&1 && echo "y" || echo "n"`
	if got, runErr := shell(context.Background(), linkCommand); runErr != nil || got != "n" {
		t.Fatalf("disabled cc-can-link = %q, %v; want n, nil", got, runErr)
	}
	if got, runErr := shell(context.Background(), probe.compilerPath+" -print-file-name=plugin"); runErr != nil || got != "" {
		t.Fatalf("disabled plugin directory = %q, %v; want empty, nil", got, runErr)
	}
	header := filepath.Join(t.TempDir(), "include", "plugin-version.h")
	if err := os.MkdirAll(filepath.Dir(header), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(header, []byte("ambient"), 0o644); err != nil {
		t.Fatal(err)
	}
	headerCommand := `{ test -e ` + header + `; } >/dev/null 2>&1 && echo "y" || echo "n"`
	if got, runErr := shell(context.Background(), headerCommand); runErr != nil || got != "n" {
		t.Fatalf("disabled plugin header = %q, %v; want n, nil", got, runErr)
	}
	if data, readErr := os.ReadFile(invocations); readErr != nil || len(data) != 0 {
		t.Fatalf("disabled policy executed compiler: %q, %v", data, readErr)
	}
}

func TestLinuxProbeShellWithToolsMeasuresAuxiliaryToolFamilies(t *testing.T) {
	for _, test := range []struct {
		name       string
		newProbe   func(*testing.T) (*LinuxToolProbe, string)
		wantLLVM   string
		wantGNUObj string
	}{
		{name: "LLVM", newProbe: func(t *testing.T) (*LinuxToolProbe, string) { return testRealToolProbe(t, "x86_64") }, wantLLVM: "y", wantGNUObj: "n"},
		{name: "GNU", newProbe: testGCCProbe, wantLLVM: "n", wantGNUObj: "y"},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe, _ := test.newProbe(t)
			shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
			if err != nil {
				t.Fatal(err)
			}
			commands := map[string]string{
				`{ ` + probe.nmPath + ` --help | head -n 1 | grep -qi llvm; } >/dev/null 2>&1 && echo "y" || echo "n"`:        test.wantLLVM,
				`{ ` + probe.archiverPath + ` --help | head -n 1 | grep -qi llvm; } >/dev/null 2>&1 && echo "y" || echo "n"`:  test.wantLLVM,
				`{ ` + probe.objcopyPath + ` --version | head -n1 | grep -qv llvm; } >/dev/null 2>&1 && echo "y" || echo "n"`: test.wantGNUObj,
			}
			for command, want := range commands {
				got, runErr := shell(context.Background(), command)
				if runErr != nil || got != want {
					t.Errorf("shell(%q) = %q, %v; want %q", command, got, runErr, want)
				}
			}
		})
	}
}

func TestLinuxProbeShellWithToolsMeasuresLegacyX86StackProtector(t *testing.T) {
	dir := t.TempDir()
	compiler := filepath.Join(dir, "gcc")
	if err := os.WriteFile(compiler, []byte("#!/bin/sh\necho 'movq %gs:40, %rax'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile, err := LinuxTargetProfileByName("x86_64")
	if err != nil {
		t.Fatal(err)
	}
	probe := &LinuxToolProbe{
		profile: profile, compilerPath: compiler, compilerFamily: "gcc", compilerName: "GCC",
		identity: "test-stack-protector", tempDir: dir, timeout: time.Second, outputLimit: linuxProbeOutputLimit,
		cache: map[string]bool{},
	}
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, bits := range []string{"32", "64"} {
		command := `{ /src/scripts/gcc-x86_` + bits + `-has-stack-protector.sh ` + compiler + `; } >/dev/null 2>&1 && echo "y" || echo "n"`
		got, runErr := shell(context.Background(), command)
		if runErr != nil || got != "y" {
			t.Errorf("shell(%q) = %q, %v; want y, nil", command, got, runErr)
		}
	}
}

func TestLinuxProbeShellWithToolsMeasuresRELR(t *testing.T) {
	dir := t.TempDir()
	compiler := filepath.Join(dir, "gcc")
	linker := filepath.Join(dir, "ld")
	archiver := filepath.Join(dir, "ar")
	nm := filepath.Join(dir, "nm")
	objcopy := filepath.Join(dir, "objcopy")
	compilerScript := `#!/bin/sh
case " $* " in
  *' --version '*) echo 'gcc (GCC) 15.2.0'; exit 0 ;;
  *' -E -P -x c - '*) echo 'GCC 15 2 0'; exit 0 ;;
  *' -Wa,--version '*) echo 'GNU assembler (GNU Binutils) 2.44'; exit 0 ;;
esac
out=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; out="$1"; fi
  shift
done
[ -z "$out" ] || : > "$out"
`
	linkerScript := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'GNU ld (GNU Binutils) 2.44'; exit 0; fi
out=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; out="$1"; fi
  shift
done
[ -z "$out" ] || : > "$out"
`
	auxiliaryScripts := map[string]string{
		archiver: "#!/bin/sh\necho 'GNU ar (GNU Binutils) 2.44'\n",
		nm: `#!/bin/sh
if [ "$1" = "--version" ] || [ "$1" = "--help" ]; then echo 'GNU nm (GNU Binutils) 2.44'; fi
`,
		objcopy: `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'GNU objcopy (GNU Binutils) 2.44'; exit 0; fi
for last do :; done
: > "$last"
`,
	}
	for path, contents := range auxiliaryScripts {
		if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(compiler, []byte(compilerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(linker, []byte(linkerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	probe, err := NewLinuxToolProbe(LinuxToolProbeOptions{
		Profile: "x86_64", Architecture: "x86", TargetTriple: "x86_64-linux-gnu",
		CompilerPath: compiler, LinkerPath: linker, ArchiverPath: archiver, NMPath: nm, ObjcopyPath: objcopy,
		TempDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	command := `env "CC=` + compiler + `" "LD=` + linker + `" "NM=` + nm + `" "OBJCOPY=` + objcopy + `" /src/scripts/tools-support-relr.sh`
	wrapped := `{ ` + command + `; } >/dev/null 2>&1 && echo "y" || echo "n"`
	got, err := shell(context.Background(), wrapped)
	if err != nil || got != "y" {
		t.Fatalf("shell(%q) = %q, %v; want y, nil", command, got, err)
	}
	mismatched := strings.Replace(command, `"NM=`+nm+`"`, `"NM=/undeclared/nm"`, 1)
	if _, err := shell(context.Background(), `{ `+mismatched+`; } >/dev/null 2>&1 && echo "y" || echo "n"`); err == nil || !strings.Contains(err.Error(), "unsupported measured") {
		t.Fatalf("mismatched RELR tools error = %v", err)
	}
}

func TestLinuxToolProbeUsesConfiguredCompilerPrefix(t *testing.T) {
	probe, invocations := testGCCProbe(t)
	probe.compilerArgs = []string{"-DTOOLCHAIN_PREFIX_REPLAYED=1", "-B/toolchain/bin"}
	supported, err := probe.SupportsOption(context.Background(), "cc_option", []string{"-fexample"}, nil)
	if err != nil || !supported {
		t.Fatalf("SupportsOption() = %v, %v", supported, err)
	}
	data, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	invocation := string(data)
	for _, want := range []string{"-DTOOLCHAIN_PREFIX_REPLAYED=1", "-B/toolchain/bin", "-fexample"} {
		if !strings.Contains(invocation, want) {
			t.Fatalf("compiler invocation %q does not contain %q", invocation, want)
		}
	}
}

func TestLinuxToolProbeGCCOmitsClangTargetFlag(t *testing.T) {
	probe, invocations := testGCCProbe(t)
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	command := `{ ` + probe.compilerPath + ` -Werror -fno-omit-frame-pointer -c -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`
	got, err := shell(context.Background(), command)
	if err != nil || got != "y" {
		t.Fatalf("shell(%q) = %q, %v; want y, nil", command, got, err)
	}
	data, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	args := string(data)
	if strings.Contains(args, "--target=") {
		t.Fatalf("GCC probe arguments contain Clang target flag: %q", args)
	}
	for _, want := range []string{"-Werror", "-fno-omit-frame-pointer", "-x c", "-c"} {
		if !strings.Contains(args, want) {
			t.Fatalf("GCC probe arguments %q do not contain %q", args, want)
		}
	}
}

func TestLinuxProbeShellWithToolsRecognizesTargetPrefixedGCC(t *testing.T) {
	probe, invocations := testGCCProbeWithName(t, "x86_64-linux-gnu-gcc")
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	command := `{ ` + probe.compilerPath + ` -Werror -fno-omit-frame-pointer -c -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`
	got, err := shell(context.Background(), command)
	if err != nil || got != "y" {
		t.Fatalf("shell(%q) = %q, %v; want y, nil", command, got, err)
	}
	if data, readErr := os.ReadFile(invocations); readErr != nil || !strings.Contains(string(data), "-fno-omit-frame-pointer") {
		t.Fatalf("target-prefixed GCC invocations = %q, %v", data, readErr)
	}
}

func TestLinuxProbeShellWithToolsMeasuresHostNativeMarch(t *testing.T) {
	probe, counter := testRealToolProbe(t, "x86_64")
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	command := `{ clang -Werror -march=native -c -x c /dev/null -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`
	got, err := shell(context.Background(), command)
	if err != nil || got != "y" {
		t.Fatalf("shell(%q) = %q, %v; want y, nil", command, got, err)
	}
	data, readErr := os.ReadFile(counter)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(data) == 0 {
		t.Fatal("host-native probe did not execute the selected compiler")
	}
}

func TestLinuxToolProbeMeasuresSafeKbuildAssemblerSource(t *testing.T) {
	dir := t.TempDir()
	clang := filepath.Join(dir, "clang")
	lld := filepath.Join(dir, "ld.lld")
	clangScript := `#!/bin/sh
case " $* " in
  *' --version '*) echo 'clang version 22.1.8'; exit 0 ;;
  *' -E -P -x c - '*) echo 'Clang 22 1 8'; exit 0 ;;
  *' -### '*) echo '"clang" "-cc1as"' >&2; exit 0 ;;
esac
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
	opts := LinuxToolProbeOptions{
		Profile: "armv7", Architecture: "arm", TargetTriple: "arm-linux-gnueabi",
		CompilerPath: clang, LinkerPath: lld, TempDir: dir,
	}
	addAuxiliaryProbeTools(t, dir, "llvm", &opts)
	probe, err := NewLinuxToolProbe(opts)
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

func TestLinuxToolProbeEnforcesLinuxMinimumLLVMVersion(t *testing.T) {
	for _, test := range []struct {
		name         string
		clangVersion string
		lldVersion   string
		wantError    string
	}{
		{name: "minimum accepted", clangVersion: "15.0.0", lldVersion: "15.0.0"},
		{name: "newer accepted", clangVersion: "21.1.8", lldVersion: "21.1.8"},
		{name: "old clang", clangVersion: "14.0.6", lldVersion: "15.0.0", wantError: "clang version"},
		{name: "old lld", clangVersion: "15.0.0", lldVersion: "14.0.6", wantError: "ld.lld version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			clang := filepath.Join(dir, "clang")
			lld := filepath.Join(dir, "ld.lld")
			writeProbeTool(t, clang, "clang version "+test.clangVersion, filepath.Join(dir, "count"))
			writeProbeTool(t, lld, "LLD "+test.lldVersion, filepath.Join(dir, "count"))
			opts := LinuxToolProbeOptions{
				Profile: "x86_64", Architecture: "x86", TargetTriple: "x86_64-linux-gnu",
				CompilerPath: clang, LinkerPath: lld,
			}
			addAuxiliaryProbeTools(t, dir, "llvm", &opts)
			_, err := NewLinuxToolProbe(opts)
			if test.wantError == "" && err != nil {
				t.Fatalf("NewLinuxToolProbe() error = %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError) || !strings.Contains(err.Error(), "want at least")) {
				t.Fatalf("NewLinuxToolProbe() error = %v, want %q minimum error", err, test.wantError)
			}
		})
	}
}

func TestLinuxToolProbeAllowsIndependentCompilerAndLinkerFamilies(t *testing.T) {
	for _, test := range []struct {
		name            string
		compilerVersion string
		linkerVersion   string
		wantCompiler    string
		wantLinker      string
	}{
		{name: "Clang with GNU ld", compilerVersion: "clang version 22.1.8", linkerVersion: "GNU ld (GNU Binutils) 2.44", wantCompiler: "clang", wantLinker: "BFD"},
		{name: "GCC with LLD", compilerVersion: "gcc (GCC) 15.2.0", linkerVersion: "LLD version 22.1.8", wantCompiler: "gcc", wantLinker: "LLD"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			compiler := filepath.Join(dir, test.wantCompiler)
			macroIdentity := map[string]string{"clang": "Clang 22 1 8", "gcc": "GCC 15 2 0"}[test.wantCompiler]
			compilerScript := "#!/bin/sh\ncase \" $* \" in\n  *' --version '*) echo '" + test.compilerVersion + "'; exit 0 ;;\n  *' -E -P -x c - '*) echo '" + macroIdentity + "'; exit 0 ;;\nesac\n"
			if test.wantCompiler == "gcc" {
				compilerScript += "case \" $* \" in *' -Wa,--version '*) echo 'GNU assembler (GNU Binutils) 2.44';; esac\n"
			} else {
				compilerScript += "case \" $* \" in *' -### '*) echo '\"clang\" \"-cc1as\"' >&2;; esac\n"
			}
			if err := os.WriteFile(compiler, []byte(compilerScript), 0o755); err != nil {
				t.Fatal(err)
			}
			linker := filepath.Join(dir, "ld")
			if err := os.WriteFile(linker, []byte("#!/bin/sh\necho '"+test.linkerVersion+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			opts := LinuxToolProbeOptions{
				Profile: "x86_64", Architecture: "x86", TargetTriple: "x86_64-linux-gnu",
				CompilerPath: compiler, LinkerPath: linker,
			}
			addAuxiliaryProbeTools(t, dir, "llvm", &opts)
			probe, err := NewLinuxToolProbe(opts)
			if err != nil {
				t.Fatal(err)
			}
			if probe.compilerFamily != test.wantCompiler || probe.linkerName != test.wantLinker {
				t.Fatalf("families = %q/%q, want %q/%q", probe.compilerFamily, probe.linkerName, test.wantCompiler, test.wantLinker)
			}
		})
	}
}

func TestLinuxToolProbeRejectsOldGCCAndBinutils(t *testing.T) {
	for _, test := range []struct {
		name       string
		gccVersion string
		asVersion  string
		ldVersion  string
		want       string
	}{
		{name: "old GCC", gccVersion: "7.5.0", asVersion: "2.44", ldVersion: "2.44", want: "GCC version"},
		{name: "old assembler", gccVersion: "15.2.0", asVersion: "2.29", ldVersion: "2.44", want: "assembler version"},
		{name: "old linker", gccVersion: "15.2.0", asVersion: "2.44", ldVersion: "2.29", want: "ld version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			gcc := filepath.Join(dir, "gcc")
			versionParts := strings.Split(test.gccVersion, ".")
			gccScript := "#!/bin/sh\ncase \" $* \" in\n  *' -E -P -x c - '*) echo 'GCC " + strings.Join(versionParts, " ") + "' ;;\n  *' -Wa,--version '*) echo 'GNU assembler (GNU Binutils) " + test.asVersion + "' ;;\n  *) echo 'gcc (GCC) " + test.gccVersion + "' ;;\nesac\n"
			if err := os.WriteFile(gcc, []byte(gccScript), 0o755); err != nil {
				t.Fatal(err)
			}
			ld := filepath.Join(dir, "ld")
			if err := os.WriteFile(ld, []byte("#!/bin/sh\necho 'GNU ld (GNU Binutils) "+test.ldVersion+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			opts := LinuxToolProbeOptions{
				Profile:      "x86_64",
				Architecture: "x86",
				TargetTriple: "x86_64-linux-gnu",
				CompilerPath: gcc,
				LinkerPath:   ld,
			}
			addAuxiliaryProbeTools(t, dir, "gnu", &opts)
			_, err := NewLinuxToolProbe(opts)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewLinuxToolProbe() error = %v, want %q", err, test.want)
			}
		})
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

func TestLinuxProbeShellWithToolsPreservesSourceProbeModeAndWarnings(t *testing.T) {
	probe, invocations := testGCCProbe(t)
	shell, err := LinuxProbeShellWithTools(probe, LinuxProbeDefaultRustcVersion, LinuxProbeDefaultRustcLLVMVersion)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		command    string
		wantMode   string
		wantWerror bool
	}{
		{
			name:     "assembly output without warnings as errors",
			command:  `{ echo 'int __seg_fs fs; int __seg_gs gs;' | ` + probe.compilerPath + ` -x c - -S -o /dev/null; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			wantMode: "-S",
		},
		{
			name:       "object output with warnings as errors",
			command:    `{ echo '__attribute__((no_profile_instrument_function)) int x();' | ` + probe.compilerPath + ` -x c - -c -o /dev/null -Werror; } >/dev/null 2>&1 && echo "y" || echo "n"`,
			wantMode:   "-c",
			wantWerror: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(invocations, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			got, runErr := shell(context.Background(), test.command)
			if runErr != nil || got != "y" {
				t.Fatalf("shell() = %q, %v; want y, nil", got, runErr)
			}
			data, err := os.ReadFile(invocations)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Fields(string(data))
			contains := func(want string) bool {
				for _, arg := range args {
					if arg == want {
						return true
					}
				}
				return false
			}
			if !contains(test.wantMode) {
				t.Fatalf("source probe args %q do not contain mode %q", data, test.wantMode)
			}
			otherMode := map[string]string{"-c": "-S", "-S": "-c"}[test.wantMode]
			if contains(otherMode) {
				t.Fatalf("source probe args %q unexpectedly contain mode %q", data, otherMode)
			}
			if gotWerror := contains("-Werror"); gotWerror != test.wantWerror {
				t.Fatalf("source probe args %q contain -Werror = %v, want %v", data, gotWerror, test.wantWerror)
			}
		})
	}
}

func TestLinuxProbeShellWithToolsDecodesAssemblerPrintfSource(t *testing.T) {
	dir := t.TempDir()
	clang := filepath.Join(dir, "clang")
	lld := filepath.Join(dir, "ld.lld")
	captured := filepath.Join(dir, "source")
	clangScript := `#!/bin/sh
case " $* " in
  *' --version '*) echo 'clang version 22.1.8'; exit 0 ;;
  *' -E -P -x c - '*) echo 'Clang 22 1 8'; exit 0 ;;
  *' -### '*) echo '"clang" "-cc1as"' >&2; exit 0 ;;
esac
/bin/cat > '` + captured + `'
`
	if err := os.WriteFile(clang, []byte(clangScript), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProbeTool(t, lld, "LLD version 22.1.8", filepath.Join(dir, "count"))
	opts := LinuxToolProbeOptions{
		Profile: "x86_64", Architecture: "x86", TargetTriple: "x86_64-linux-gnu",
		CompilerPath: clang, LinkerPath: lld, TempDir: dir,
	}
	addAuxiliaryProbeTools(t, dir, "llvm", &opts)
	probe, err := NewLinuxToolProbe(opts)
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
case " $* " in
  *' --version '*) echo 'clang version 22.1.8'; exit 0 ;;
  *' -E -P -x c - '*) echo 'Clang 22 1 8'; exit 0 ;;
  *' -### '*) echo '"clang" "-cc1as"' >&2; exit 0 ;;
esac
` + test.body + `
`
			if err := os.WriteFile(clang, []byte(tool), 0o755); err != nil {
				t.Fatal(err)
			}
			lldTool := strings.Replace(tool, "clang version", "LLD version", 1)
			if err := os.WriteFile(lld, []byte(lldTool), 0o755); err != nil {
				t.Fatal(err)
			}
			opts := LinuxToolProbeOptions{
				Profile: "x86_64", Architecture: "x86", TargetTriple: "x86_64-linux-gnu",
				CompilerPath: clang, LinkerPath: lld, TempDir: dir, Timeout: 50 * time.Millisecond, OutputLimit: 32,
			}
			addAuxiliaryProbeTools(t, dir, "llvm", &opts)
			probe, err := NewLinuxToolProbe(opts)
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
	shell := &linuxProbeShell{}
	for _, compiler := range []string{"$CC", "$(CC)", "/pinned/bin/clang"} {
		command := "echo 'int foo(void) { return 0; }' | " + compiler + " $(CLANG_FLAGS) -x c - -c -o /dev/null -Werror"
		source, mode, candidate, err := shell.parseLinuxSourceProbe(command)
		if err != nil {
			t.Fatalf("parseLinuxSourceProbe(%q): %v", command, err)
		}
		if source != "int foo(void) { return 0; }" {
			t.Fatalf("source = %q", source)
		}
		if mode != "-c" {
			t.Fatalf("mode = %q, want -c", mode)
		}
		if got, want := strings.Join(candidate, " "), "-fintegrated-as -Werror"; got != want {
			t.Fatalf("candidate = %q, want %q", got, want)
		}
	}
}

func TestParseLinuxSourceProbeAcceptsSelectedTargetPrefixedGCC(t *testing.T) {
	shell := &linuxProbeShell{
		toolProbe: &LinuxToolProbe{compilerPath: "/pinned/bin/x86_64-linux-gnu-gcc"},
	}
	command := "echo 'int foo(void) { return 0; }' | x86_64-linux-gnu-gcc -x c - -c -o /dev/null -Werror"
	source, mode, candidate, err := shell.parseLinuxSourceProbe(command)
	if err != nil {
		t.Fatal(err)
	}
	if source != "int foo(void) { return 0; }" {
		t.Fatalf("source = %q", source)
	}
	if mode != "-c" {
		t.Fatalf("mode = %q, want -c", mode)
	}
	if got, want := strings.Join(candidate, " "), "-Werror"; got != want {
		t.Fatalf("candidate = %q, want %q", got, want)
	}
}

func TestParseLinuxSourceProbeUnquotesVPCLMULImmediate(t *testing.T) {
	shell := &linuxProbeShell{}
	command := `printf "%b\n" "vpclmulqdq \$0x10,%ymm0,%ymm1,%ymm2" | clang -fintegrated-as -Wa,--fatal-warnings -c -x assembler-with-cpp -o /dev/null -`
	source, mode, candidate, err := shell.parseLinuxSourceProbe(command)
	if err != nil {
		t.Fatal(err)
	}
	if want := `vpclmulqdq $0x10,%ymm0,%ymm1,%ymm2`; source != want {
		t.Fatalf("source = %q, want %q", source, want)
	}
	if mode != "-c" {
		t.Fatalf("mode = %q, want -c", mode)
	}
	if got, want := strings.Join(candidate, " "), "-fintegrated-as -Wa,--fatal-warnings"; got != want {
		t.Fatalf("candidate = %q, want %q", got, want)
	}
}

func TestKnownRELRProbeAcceptsPinnedToolPaths(t *testing.T) {
	command := `env "CC=/pinned/bin/clang" "LD=/pinned/bin/ld.lld" "NM=llvm-nm" "OBJCOPY=llvm-objcopy" /src/scripts/tools-support-relr.sh`
	shell := &linuxProbeShell{
		toolProbe: &LinuxToolProbe{
			compilerPath: "/pinned/bin/clang",
			linkerPath:   "/pinned/bin/ld.lld",
			nmPath:       "/tools/llvm-nm",
			objcopyPath:  "/tools/llvm-objcopy",
		},
	}
	nm, objcopy, recognized, err := shell.relrProbeTools(command)
	if err != nil || !recognized || nm != "llvm-nm" || objcopy != "llvm-objcopy" {
		t.Fatalf("relrProbeTools(%q) = %q, %q, %v, %v", command, nm, objcopy, recognized, err)
	}
}
