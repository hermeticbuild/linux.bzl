package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
	"github.com/hermeticbuild/linux.bzl/internal/toolsetpath"
)

func TestMain(testMain *testing.M) {
	if handled, exitCode := executeScriptReplayProxyMode(os.Args[1:], os.Stderr); handled {
		os.Exit(exitCode)
	}
	os.Exit(testMain.Run())
}

func writeExecutable(t *testing.T, directory, name, contents string) string {
	t.Helper()
	filename := filepath.Join(directory, name)
	if err := os.WriteFile(filename, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return filename
}

func writeReplayRuntime(t *testing.T, directory string) string {
	t.Helper()
	return writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then
	printf '%s\n' '[' sh
	exit 0
fi
if [ "$1" != sh ]; then
	exit 64
fi
shift
exec /bin/sh "$@"
`)
}

func TestRunScriptUsesDeclaredSourceStreamsAndExternalTool(t *testing.T) {
	directory := t.TempDir()
	interpreter := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then
	printf '%s\n' '[' '[[' sh
	exit 0
fi
if [ "$1" = sh ]; then
	shift
	exec /bin/sh "$@"
fi
exit 64
`)
	external := writeExecutable(t, directory, "declared-external", `#!/bin/sh
IFS= read -r value
printf '%s|%s|%s|%s|%s\n' "$1" "$2" "$3" "$SELECTED_ENV" "$value"
`)
	script := filepath.Join(directory, "source-script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexternal \"$1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := runScript(scriptRunOptions{
		interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
		script: script, scriptArgs: []string{"argument"}, tools: map[string]string{"external": external},
		toolContracts: map[string]toolaction.Contract{
			"external": {
				Arguments:   []string{"prefix value", toolaction.KbuildArgumentsSentinel, "suffix'value"},
				Environment: map[string]string{"SELECTED_ENV": "configured ' value"},
			},
		},
		stdin: strings.NewReader("streamed-input\n"), stdout: &stdout, stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("runScript() failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "prefix value|argument|suffix'value|configured ' value|streamed-input\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunScriptConsumesEvaluatedScriptFromStdin(t *testing.T) {
	directory := t.TempDir()
	interpreter := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then
	printf '%s\n' sh
	exit 0
fi
if [ "$1" = sh ]; then
	shift
	exec /bin/sh "$@"
fi
exit 64
`)
	var stdout, stderr bytes.Buffer
	err := runScript(scriptRunOptions{
		interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
		scriptStdin: true,
		stdin: strings.NewReader(`if IFS= read -r unexpected; then exit 79; fi
printf '%s\n' stdin-script-ran
`),
		stdout: &stdout, stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("runScript() failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "stdin-script-ran\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunScriptDispatchesCompilerDriverContractAndPreservesRuntimeTools(t *testing.T) {
	directory := t.TempDir()
	runtime := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then printf '%s\n' sh; exit 0; fi
if [ "$1" = sh ]; then shift; exec /bin/sh "$@"; fi
exit 64
`)
	compiler := writeExecutable(t, directory, "selected-cc", `#!/bin/sh
out=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then shift; out=$1; break; fi
  case "$1" in -o?*) out=${1#-o}; break ;; esac
  shift
done
[ -n "$out" ] || exit 65
printf '%s' "$SELECTED_MODE" > "$out"
if [ "$SELECTED_MODE" = link ]; then ld "$out"; fi
`)
	runtimeTools := filepath.Join(directory, "runtime-tools")
	if err := os.Mkdir(runtimeTools, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, runtimeTools, "ld", "#!/bin/sh\nprintf '%s' '+runtime-ld' >> \"$1\"\n")
	script := filepath.Join(directory, "source-script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncc -c source.c -o \"$1\"\ncc first.o -o \"$2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	compileOutput := filepath.Join(directory, "compile.out")
	linkOutput := filepath.Join(directory, "link.out")
	err := runScript(scriptRunOptions{
		interpreter: runtime, interpreterArgs: []string{"sh"}, multicall: runtime,
		script: script, scriptArgs: []string{compileOutput, linkOutput},
		tools: map[string]string{"cc": compiler},
		toolContracts: map[string]toolaction.Contract{
			"cc":      {Arguments: []string{toolaction.KbuildArgumentsSentinel}, Environment: map[string]string{"SELECTED_MODE": "compile"}},
			"cc-link": {Arguments: []string{toolaction.KbuildArgumentsSentinel}, Environment: map[string]string{"SELECTED_MODE": "link"}},
		},
		runtimeToolPath: runtimeTools,
		stdout:          ioDiscard{}, stderr: ioDiscard{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for filename, want := range map[string]string{compileOutput: "compile", linkOutput: "link+runtime-ld"} {
		got, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", filepath.Base(filename), got, want)
		}
	}
}

func TestRunScriptDoesNotSearchAmbientPath(t *testing.T) {
	directory := t.TempDir()
	interpreter := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then
	printf '%s\n' sh
	exit 0
fi
shift
exec /bin/sh "$@"
`)
	script := filepath.Join(directory, "source-script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nuname\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runScript(scriptRunOptions{
		interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
		script: script, tools: map[string]string{}, stdout: ioDiscard{}, stderr: ioDiscard{},
	})
	if err == nil {
		t.Fatal("runScript() found an undeclared ambient program")
	}
}

func TestRunScriptRuntimeAppletOverridesMulticallCommand(t *testing.T) {
	directory := t.TempDir()
	runtime := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then printf '%s\n' find sh; exit 0; fi
if [ "$1" = sh ]; then shift; exec /bin/sh "$@"; fi
if [ "$1" = find ]; then printf 'base-find\n'; exit 0; fi
exit 64
`)
	override := writeExecutable(t, directory, "selected-multicall", `#!/bin/sh
[ "${0##*/}" = find ] || exit 65
printf 'selected-find:%s\n' "$1"
`)
	script := filepath.Join(directory, "source-script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nfind source-tree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err := runScript(scriptRunOptions{
		interpreter: runtime, interpreterArgs: []string{"sh"}, multicall: runtime,
		script: script, applets: map[string]string{"find": override}, tools: map[string]string{},
		toolContracts: map[string]toolaction.Contract{
			scriptAppletRolePrefix + "find": {Arguments: []string{}, Environment: map[string]string{}},
		},
		stdout: &stdout, stderr: ioDiscard{},
	})
	if err != nil {
		t.Fatalf("runScript() failed: %v", err)
	}
	if got, want := stdout.String(), "selected-find:source-tree\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunScriptRequiresSelectedMulticallApplets(t *testing.T) {
	directory := t.TempDir()
	runtime := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then printf '%s\n' grep sh; exit 0; fi
if [ "$1" = sh ]; then shift; exec /bin/sh "$@"; fi
exit 64
`)
	base := scriptRunOptions{
		interpreter: runtime, interpreterArgs: []string{"sh"}, multicall: runtime,
		scriptContent: ":\n", requiredApplets: []string{"grep"},
		tools: map[string]string{}, toolContracts: map[string]toolaction.Contract{},
		stdout: ioDiscard{}, stderr: ioDiscard{},
	}
	if err := runScript(base); err != nil {
		t.Fatalf("runScript() rejected declared grep applet: %v", err)
	}
	base.requiredApplets = []string{"sort"}
	if err := runScript(base); err == nil || !strings.Contains(err.Error(), `does not provide required applet "sort"`) {
		t.Fatalf("runScript() missing-appet error = %v", err)
	}
}

func TestRunScriptFallbackPassesThroughSuccessfulOutput(t *testing.T) {
	directory := t.TempDir()
	runtime := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then printf '%s\n' sh; exit 0; fi
if [ "$1" = sh ]; then shift; exec /bin/sh "$@"; fi
exit 64
`)
	var stdout bytes.Buffer
	err := runScriptWithFallback(scriptRunOptions{
		interpreter: runtime, interpreterArgs: []string{"sh"}, multicall: runtime,
		scriptContent: "printf '%s\\n' measured-output\n",
		stdout:        &stdout, stderr: ioDiscard{},
	}, scriptRunFallback{enabled: true, timeout: time.Second, stdout: []byte("unsafe\n")})
	if err != nil {
		t.Fatalf("runScriptWithFallback() failed: %v", err)
	}
	if got, want := stdout.String(), "measured-output\n"; got != want {
		t.Fatalf("stdout = %q, want successful output %q", got, want)
	}
}

func TestRunScriptFallbackReplacesPartialOutputAfterTimeout(t *testing.T) {
	directory := t.TempDir()
	runtime := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then printf '%s\n' sh; exit 0; fi
if [ "$1" = sh ]; then shift; exec /bin/sh "$@"; fi
exit 64
`)
	var stdout bytes.Buffer
	started := time.Now()
	err := runScriptWithFallback(scriptRunOptions{
		interpreter: runtime, interpreterArgs: []string{"sh"}, multicall: runtime,
		scriptContent: "printf partial-output; while :; do :; done\n",
		stdout:        &stdout, stderr: ioDiscard{},
	}, scriptRunFallback{enabled: true, timeout: 100 * time.Millisecond, stdout: []byte("unsafe\n")})
	if err != nil {
		t.Fatalf("runScriptWithFallback() failed: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("timed-out script returned after %v, want a prompt fallback", elapsed)
	}
	if got, want := stdout.String(), "unsafe\n"; got != want {
		t.Fatalf("stdout = %q, want only fallback %q", got, want)
	}
}

func TestRunScriptFallbackHandlesSetupFailure(t *testing.T) {
	directory := t.TempDir()
	runtime := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then printf '%s\n' sh; exit 0; fi
if [ "$1" = sh ]; then shift; exec /bin/sh "$@"; fi
exit 64
`)
	var stdout bytes.Buffer
	err := runScriptWithFallback(scriptRunOptions{
		interpreter: runtime, interpreterArgs: []string{"sh"}, multicall: runtime,
		scriptContent:   ":\n",
		requiredApplets: []string{"missing"},
		stdout:          &stdout, stderr: ioDiscard{},
	}, scriptRunFallback{enabled: true, timeout: time.Second, stdout: []byte("unsafe\n")})
	if err != nil {
		t.Fatalf("runScriptWithFallback() failed: %v", err)
	}
	if got, want := stdout.String(), "unsafe\n"; got != want {
		t.Fatalf("stdout = %q, want setup fallback %q", got, want)
	}
}

func TestRunScriptFallbackHandlesFileSizeLimit(t *testing.T) {
	directory := t.TempDir()
	runtime := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then printf '%s\n' sh; exit 0; fi
if [ "$1" = sh ]; then shift; exec /bin/sh "$@"; fi
exit 64
`)
	const fileSizeLimit = 32 << 10
	output := filepath.Join(directory, "oversized-output")
	var inherited syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &inherited); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &inherited); err != nil {
			t.Errorf("restore test file-size limit: %v", err)
		}
	}()

	var stdout bytes.Buffer
	err := runScriptWithFallback(scriptRunOptions{
		interpreter: runtime, interpreterArgs: []string{"sh"}, multicall: runtime,
		scriptContent:    "printf '%262144s' x > \"$1\"\n",
		scriptArgs:       []string{output},
		maxFileSizeBytes: fileSizeLimit,
		stdout:           &stdout, stderr: ioDiscard{},
	}, scriptRunFallback{enabled: true, timeout: 2 * time.Second, stdout: []byte("unsafe\n")})
	if err != nil {
		t.Fatalf("runScriptWithFallback() failed: %v", err)
	}
	if got, want := stdout.String(), "unsafe\n"; got != want {
		t.Fatalf("stdout = %q, want file-size fallback %q", got, want)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("stat bounded output: %v", err)
	}
	if info.Size() > fileSizeLimit {
		t.Fatalf("bounded output size = %d, want at most %d", info.Size(), fileSizeLimit)
	}
	var restored syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &restored); err != nil {
		t.Fatal(err)
	}
	if restored != inherited {
		t.Fatalf("parent file-size limit = %#v, want inherited %#v", restored, inherited)
	}
}

func TestMaxFileSizeFlag(t *testing.T) {
	var value maxFileSizeFlag
	if err := value.Set("32768"); err != nil {
		t.Fatalf("Set() failed: %v", err)
	}
	if got, want := value.bytes, uint64(32768); got != want {
		t.Fatalf("parsed bytes = %d, want %d", got, want)
	}
	if got, want := value.String(), "32768"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}

	for _, invalid := range []string{"", "0", "-1", "1.5", "1073741825"} {
		t.Run(invalid, func(t *testing.T) {
			var value maxFileSizeFlag
			if err := value.Set(invalid); err == nil {
				t.Fatalf("Set(%q) accepted invalid max_file_size_bytes", invalid)
			}
		})
	}
}

func TestParseScriptRunFallback(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("unsafe\n"))
	fallback, err := parseScriptRunFallback(10, encoded)
	if err != nil {
		t.Fatalf("parseScriptRunFallback() failed: %v", err)
	}
	if !fallback.enabled || fallback.timeout != 10*time.Second || string(fallback.stdout) != "unsafe\n" {
		t.Fatalf("fallback = %#v", fallback)
	}
	disabled, err := parseScriptRunFallback(0, "")
	if err != nil || disabled.enabled {
		t.Fatalf("disabled fallback = %#v, %v", disabled, err)
	}

	for _, test := range []struct {
		name    string
		timeout int
		stdout  string
	}{
		{name: "timeout without fallback", timeout: 1},
		{name: "fallback without timeout", stdout: encoded},
		{name: "negative timeout", timeout: -1, stdout: encoded},
		{name: "unbounded timeout", timeout: maxScriptTimeoutSeconds + 1, stdout: encoded},
		{name: "malformed fallback", timeout: 1, stdout: "not-base64"},
		{name: "oversized fallback", timeout: 1, stdout: strings.Repeat("A", base64.StdEncoding.EncodedLen(maxFallbackStdoutBytes)+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseScriptRunFallback(test.timeout, test.stdout); err == nil {
				t.Fatal("parseScriptRunFallback() accepted invalid flags")
			}
		})
	}
}

func TestValidateToolContractsRejectsRuntimeAppletWrapperPolicy(t *testing.T) {
	err := validateToolContracts(
		map[string]string{},
		map[string]string{"find": "/selected/toybox"},
		map[string]toolaction.Contract{
			scriptAppletRolePrefix + "find": {
				Arguments:   []string{"find", toolaction.KbuildArgumentsSentinel},
				Environment: map[string]string{},
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "empty action contract") {
		t.Fatalf("validateToolContracts() error = %v, want runtime applet wrapper rejection", err)
	}
}

func TestRunScriptAcceptsBoundRuntimeToolContract(t *testing.T) {
	directory := t.TempDir()
	runtime := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then
	printf '%s\n' sh
	exit 0
fi
if [ "$1" = sh ]; then
	shift
	exec /bin/sh "$@"
fi
exit 64
`)
	script := filepath.Join(directory, "source-script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf contract-ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err := runScript(scriptRunOptions{
		interpreter: runtime, interpreterArgs: []string{"sh"}, multicall: runtime,
		script: script,
		tools:  map[string]string{"script-runtime": runtime},
		toolContracts: map[string]toolaction.Contract{
			"script-runtime": {Arguments: []string{}, Environment: map[string]string{}},
		},
		stdout: &stdout, stderr: ioDiscard{},
	})
	if err != nil {
		t.Fatalf("runScript() rejected its bound runtime contract: %v", err)
	}
	if got, want := stdout.String(), "contract-ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunScriptUsesWritableTemporaryRuntimeFromReadOnlySourceDirectory(t *testing.T) {
	directory := t.TempDir()
	interpreter := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then
	printf '%s\n' sh
	exit 0
fi
shift
exec /bin/sh "$@"
`)
	script := filepath.Join(directory, "source-script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf source-tree-ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Chdir(workingDirectory)
		_ = os.Chmod(directory, 0o700)
	}()
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err = runScript(scriptRunOptions{
		interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
		script: script, tools: map[string]string{}, stdout: &stdout, stderr: ioDiscard{},
	})
	if err != nil {
		t.Fatalf("runScript() from read-only source directory failed: %v", err)
	}
	if got, want := stdout.String(), "source-tree-ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRunScriptExecutesEvaluatedContentInCallerWorkingDirectory(t *testing.T) {
	directory := t.TempDir()
	interpreter := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then
	printf '%s\n' sh
	exit 0
fi
if [ "$1" = sh ]; then
	shift
	exec /bin/sh "$@"
fi
exit 64
`)
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(workingDirectory) }()
	err = runScript(scriptRunOptions{
		interpreter:     interpreter,
		interpreterArgs: []string{"sh"},
		multicall:       interpreter,
		scriptContent:   "#!/bin/sh\nset -e\n: > generated\n",
		tools:           map[string]string{},
		toolContracts:   map[string]toolaction.Contract{},
		stdout:          ioDiscard{},
		stderr:          ioDiscard{},
	})
	if err != nil {
		t.Fatalf("runScript(evaluated content) failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "generated")); err != nil {
		t.Fatalf("evaluated content did not run in caller working directory: %v", err)
	}
}

func TestExpandScriptTreeBindingsPreservesProvenanceMarkedLiteral(t *testing.T) {
	const script = `printf '%s %s' '${tree:prep}' ${tree:kernel}`
	literal := strings.Index(script, "${tree:prep}")
	got, err := expandScriptTreeBindingsWithLiteralOffsets(
		script,
		map[string]string{"kernel": "/declared/kernel"},
		map[int]bool{literal: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := `printf '%s %s' '${tree:prep}' /declared/kernel`; got != want {
		t.Fatalf("expanded script=%q, want %q", got, want)
	}
}

func TestRunScriptResolvesTypedToolsetPathsAfterLiteralOffsets(t *testing.T) {
	executionRoot := filepath.Join(t.TempDir(), "mapped $(printf unsafe) `printf unsafe` root's files")
	canonicalHeader := "external/gcc/include/arm_neon.h"
	include := filepath.Join(executionRoot, "bazel-out", "arm64-fastbuild", "genfiles", "external", "gcc", "include")
	if err := os.MkdirAll(include, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(include, "arm_neon.h"), []byte("intrinsic"), 0o644); err != nil {
		t.Fatal(err)
	}
	interpreter := writeExecutable(t, executionRoot, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then
	printf '%s\n' sh
	exit 0
fi
if [ "${0##*/}" = sh ]; then
	exec /bin/sh "$@"
fi
if [ "$1" = sh ]; then
	shift
	exec /bin/sh "$@"
fi
exit 64
`)
	privatePath, err := toolaction.EncodeExecutionRootProvenancePath("target", canonicalHeader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := toolaction.KbuildToolsetManifest{
		Schema:        toolaction.KbuildToolsetManifestSchema,
		Scope:         "target",
		Actions:       map[string][]string{"cc": {toolaction.KbuildArgumentsSentinel}},
		Tools:         map[string]string{"cc": canonicalHeader},
		Closure:       []string{canonicalHeader},
		ArtifactKinds: map[string]string{canonicalHeader: toolaction.KbuildToolsetArtifactGeneratedFile},
		ArtifactRoots: map[string]toolaction.KbuildToolsetArtifactRoot{
			canonicalHeader: {Root: "root-00000000", Path: canonicalHeader},
		},
		Roots:         map[string]string{"root-00000000": canonicalHeader},
		Environments:  map[string]map[string]string{"cc": {}},
		MakeVariables: map[string]string{},
		Requirements:  map[string]map[string]string{"cc": {}},
	}
	identity, err := manifest.Identity()
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(executionRoot, "target-toolset.json")
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := toolsetpath.LoadFlags(
		executionRoot,
		filepath.Join(executionRoot, "parent-projection"),
		[]string{"target=" + identity},
		[]string{"target=" + manifestPath},
		[]string{"target=root-00000000=" + filepath.Join(include, "arm_neon.h")},
	)
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := resolver.CreateHandoff(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scriptContent := "#!/bin/sh\nvalue=\nIFS= read -r value < " + privatePath + " || :\nprintf '%s|%s|%s' \"$value\" '" + toolaction.ExecutionRootMarker + "' '${tree:literal}'\nprintf '|single:%s|double:%s' 'prefix " + privatePath + " suffix' \"prefix " + privatePath + " suffix\"\ncd /\nafter_cd=\nIFS= read -r after_cd < " + privatePath + " || :\nprintf '|cwd:%s' \"$after_cd\"\nsh -c 'nested=; IFS= read -r nested < " + privatePath + " || :; printf \"|nested:%s\" \"$nested\"'\neval 'evaluated=; IFS= read -r evaluated < " + privatePath + " || :'\nprintf '|eval:%s' \"$evaluated\"\n"
	literalOffset := strings.Index(scriptContent, "${tree:literal}")
	if literalOffset < 0 {
		t.Fatal("test script omits literal tree marker")
	}
	var stdout, stderr bytes.Buffer
	err = runScript(scriptRunOptions{
		interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
		toolsetHandoff: handoff,
		scriptContent:  scriptContent, literalTreeOffsets: map[int]bool{literalOffset: true},
		tools: map[string]string{}, toolContracts: map[string]toolaction.Contract{},
		stdout: &stdout, stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("runScript() failed: %v\nstderr: %s", err, stderr.String())
	}
	visibleHeader := toolsetpath.ShellAliasRootPath + "/aliases/target/" + identity + "/" + canonicalHeader
	if got, want := stdout.String(), "intrinsic|"+toolaction.ExecutionRootMarker+"|${tree:literal}|single:prefix "+visibleHeader+" suffix|double:prefix "+visibleHeader+" suffix|cwd:intrinsic|nested:intrinsic|eval:intrinsic"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	err = runScript(scriptRunOptions{
		interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
		toolsetHandoff: handoff,
		scriptContent:  "#!/bin/sh\ncat <\\\n<EOF\n" + privatePath + "\nEOF\n",
		tools:          map[string]string{}, toolContracts: map[string]toolaction.Contract{},
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "heredoc") {
		t.Fatalf("runScript(continued heredoc) error = %v, want heredoc rejection", err)
	}

	hostPath, err := toolaction.EncodeExecutionRootProvenancePath("host", canonicalHeader)
	if err != nil {
		t.Fatal(err)
	}
	err = runScript(scriptRunOptions{
		interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
		toolsetHandoff: handoff,
		scriptContent:  "#!/bin/sh\nprintf '%s' " + hostPath + "\n",
		tools:          map[string]string{}, toolContracts: map[string]toolaction.Contract{},
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "no typed host toolset scope") {
		t.Fatalf("runScript(unknown scope) error = %v", err)
	}
}

func TestRunScriptRejectsAmbiguousScriptSources(t *testing.T) {
	err := runScript(scriptRunOptions{script: "source.sh", scriptContent: "echo duplicate"})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("runScript() error = %v, want exclusive script source validation", err)
	}
}

func TestResolveScriptContentAcceptsRawBytesAndRejectsAmbiguousFlags(t *testing.T) {
	const raw = "#!/bin/sh\nprintf '\\\\004'\n"
	got, err := resolveScriptContent("", raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != raw {
		t.Fatalf("raw script content = %q, want %q", got, raw)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))
	got, err = resolveScriptContent("", "", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got != raw {
		t.Fatalf("decoded script content = %q, want %q", got, raw)
	}
	for _, test := range []struct {
		name, source, raw, encoded string
	}{
		{name: "source-and-raw", source: "source.sh", raw: raw},
		{name: "source-and-base64", source: "source.sh", encoded: encoded},
		{name: "raw-and-base64", raw: raw, encoded: encoded},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := resolveScriptContent(test.source, test.raw, test.encoded); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
				t.Fatalf("resolveScriptContent() error = %v, want mutual-exclusion error", err)
			}
		})
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(value []byte) (int, error) { return len(value), nil }

func TestParseToolBindingsRejectsInvalidOrRepeatedNames(t *testing.T) {
	for _, values := range [][]string{{"../escape=/tool"}, {"awk=/one", "awk=/two"}, {"missing"}} {
		if _, err := parseToolBindings(values); err == nil {
			t.Errorf("parseToolBindings(%q) succeeded", values)
		}
	}
}

func TestValidateScriptToolNameAcceptsBusyBoxTestApplets(t *testing.T) {
	for _, name := range []string{"[", "[["} {
		if err := validateScriptToolName(name); err != nil {
			t.Errorf("validateScriptToolName(%q) failed: %v", name, err)
		}
	}
	for _, name := range []string{"]", "../[", "bin/["} {
		if err := validateScriptToolName(name); err == nil {
			t.Errorf("validateScriptToolName(%q) succeeded", name)
		}
	}
}

func TestRunScriptReplayProxyAcceptsOnlyDeclaredArgumentsWithRegularOutputs(t *testing.T) {
	t.Setenv("MAKE", "make")
	directory := t.TempDir()
	interpreter := writeReplayRuntime(t, directory)
	relativeOutput := "child/generated 'file'.o"
	absoluteOutput := filepath.Join(directory, "absolute output")
	unreadableOutput := filepath.Join(directory, "unreadable output")
	if err := os.Mkdir(filepath.Join(directory, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{filepath.Join(directory, relativeOutput), absoluteOutput} {
		if err := os.WriteFile(output, []byte("materialized"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(unreadableOutput, []byte("executable"), 0o101); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadableOutput, 0o101); err != nil {
		t.Fatal(err)
	}
	manifestJSON, err := json.Marshal(scriptReplayManifest{
		Name: "make",
		Invocations: []scriptReplayInvocation{{
			Arguments: []string{"-f", "Makefile", "obj=init dir", "quoted'value"},
			Outputs:   []string{relativeOutput, absoluteOutput, unreadableOutput},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	replays, err := decodeReplayManifests([]string{base64.StdEncoding.EncodeToString(manifestJSON)})
	if err != nil {
		t.Fatalf("decodeReplayManifests() failed: %v", err)
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(workingDirectory) }()
	var stdout, stderr bytes.Buffer
	err = runScript(scriptRunOptions{
		interpreter:     interpreter,
		interpreterArgs: []string{"sh"},
		multicall:       interpreter,
		scriptContent:   "#!/bin/sh\n\"$MAKE\" -f Makefile 'obj=init dir' \"quoted'value\"\nprintf replay-ok\n",
		tools:           map[string]string{},
		toolContracts:   map[string]toolaction.Contract{},
		replays:         replays,
		stdout:          &stdout,
		stderr:          &stderr,
	})
	if err != nil {
		t.Fatalf("runScript() failed: %v\nstderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "replay-ok"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if info, err := os.Stat(unreadableOutput); err != nil {
		t.Fatal(err)
	} else if got, want := info.Mode().Perm(), os.FileMode(0o101); got != want {
		t.Fatalf("unreadable output mode = %#o, want %#o", got, want)
	}
}

func TestRunScriptReplayKeepsAuthoritativeStateOutOfRuntimeFilesystem(t *testing.T) {
	directory := t.TempDir()
	interpreter := writeReplayRuntime(t, directory)
	output := filepath.Join(directory, "generated.o")
	if err := os.WriteFile(output, []byte("materialized"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The source can read and write its same-uid runtime directory. It must find
	// neither the old authoritative state/receipt files nor expected argv/output
	// identity embedded in the executable replay wrapper.
	script := `#!/bin/sh
runtime=${PATH%/bin}
for candidate in "$runtime"/replay-state-* "$runtime"/replay-receipts; do
  [ ! -e "$candidate" ] || exit 81
done
proxy=$(command -v make)
while IFS= read -r line; do
  case "$line" in
    *expected*|*generated.o*) exit 82 ;;
  esac
done < "$proxy"
make expected
`
	var stderr bytes.Buffer
	err := runScript(scriptRunOptions{
		interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
		scriptContent: script, replays: []scriptReplayManifest{{
			Name: "make", Invocations: []scriptReplayInvocation{{Arguments: []string{"expected"}, Outputs: []string{output}}},
		}},
		tools: map[string]string{}, toolContracts: map[string]toolaction.Contract{},
		stdout: ioDiscard{}, stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("runScript() exposed writable replay authority: %v\nstderr: %s", err, stderr.String())
	}
}

func TestScriptReplayArgumentIdentityPreservesInvalidUTF8Bytes(t *testing.T) {
	raw := string([]byte{0xff, 0xfe})
	replacement := "\ufffd\ufffd"
	if err := validateReplayManifests([]scriptReplayManifest{{
		Name: "make",
		Invocations: []scriptReplayInvocation{
			{Arguments: []string{raw}},
			{Arguments: []string{replacement}},
		},
	}}, nil); err != nil {
		t.Fatalf("byte-distinct argv were treated as duplicates: %v", err)
	}

	directory := t.TempDir()
	output := filepath.Join(directory, "generated.o")
	if err := os.WriteFile(output, []byte("materialized"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareScriptReplayRuntime(scriptReplayManifest{
		Name: "make", Invocations: []scriptReplayInvocation{{Arguments: []string{raw}, Outputs: []string{output}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := startScriptReplayBroker([]*scriptReplayRuntimeManifest{prepared})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close() }()
	if err := runScriptReplayProxy(broker.endpoint, 0, []string{raw}); err != nil {
		t.Fatalf("byte-exact invalid UTF-8 argv was rejected: %v", err)
	}
	err = runScriptReplayProxy(broker.endpoint, 0, []string{replacement})
	var failure *scriptReplayProxyFailure
	if !errors.As(err, &failure) || failure.exitCode != 64 {
		t.Fatalf("replacement-rune argv error = %v, want exact status 64", err)
	}
}

func TestScriptReplayBrokerRejectsMalformedProtocolWithStatus70(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "generated.o")
	if err := os.WriteFile(output, []byte("materialized"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareScriptReplayRuntime(scriptReplayManifest{
		Name: "make", Invocations: []scriptReplayInvocation{{Arguments: []string{"expected"}, Outputs: []string{output}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := startScriptReplayBroker([]*scriptReplayRuntimeManifest{prepared})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close() }()

	var valid bytes.Buffer
	if err := writeScriptReplayRequest(&valid, 0, []string{"expected"}); err != nil {
		t.Fatal(err)
	}
	withTrailing := append(append([]byte(nil), valid.Bytes()...), 0x7f)
	tooManyArguments := make([]byte, 16)
	copy(tooManyArguments[:8], scriptReplayProtocol)
	binary.BigEndian.PutUint32(tooManyArguments[12:16], maxReplayArguments+1)
	for _, test := range []struct {
		name    string
		request []byte
	}{
		{name: "truncated", request: []byte(scriptReplayProtocol)},
		{name: "wrong identity", request: make([]byte, 16)},
		{name: "too many arguments", request: tooManyArguments},
		{name: "trailing bytes", request: withTrailing},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Net: "unix", Name: broker.endpoint})
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := writeScriptReplayBytes(connection, test.request); err != nil {
				t.Fatal(err)
			}
			if err := connection.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			exitCode, diagnostic, err := readScriptReplayResponse(connection)
			if err != nil {
				t.Fatal(err)
			}
			if exitCode != 70 || diagnostic == "" {
				t.Fatalf("malformed protocol response = (%d, %q), want status 70 with diagnostic", exitCode, diagnostic)
			}
		})
	}
	unknownErr := runScriptReplayProxy(broker.endpoint, 99, nil)
	var unknownFailure *scriptReplayProxyFailure
	if !errors.As(unknownErr, &unknownFailure) || unknownFailure.exitCode != 70 {
		t.Fatalf("unknown replay identity error = %v, want status 70", unknownErr)
	}
}

func TestRunScriptReplayProxyRejectsPreexistingOutputMutationBeforeReplay(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutation   func(string) string
		diagnostic string
	}{
		{
			name:       "content",
			mutation:   func(output string) string { return "printf modified >" + shellQuote(output) },
			diagnostic: "changed content before replay",
		},
		{
			name:       "executable bits",
			mutation:   func(output string) string { return "/bin/chmod 700 " + shellQuote(output) },
			diagnostic: "changed executable bits before replay",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			interpreter := writeReplayRuntime(t, directory)
			output := filepath.Join(directory, "generated.o")
			if err := os.WriteFile(output, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			err := runScript(scriptRunOptions{
				interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
				scriptContent: "#!/bin/sh\n" + test.mutation(output) + "\nmake expected\n", replays: []scriptReplayManifest{{
					Name: "make", Invocations: []scriptReplayInvocation{{Arguments: []string{"expected"}, Outputs: []string{output}}},
				}},
				tools: map[string]string{}, toolContracts: map[string]toolaction.Contract{},
				stdout: ioDiscard{}, stderr: &stderr,
			})
			if err == nil {
				t.Fatal("runScript() accepted a mutated preexisting replay output")
			}
			if !strings.Contains(stderr.String(), test.diagnostic) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), test.diagnostic)
			}
		})
	}
}

func TestRunScriptReplayProxyAcceptsNewOutputCreatedAtReplay(t *testing.T) {
	directory := t.TempDir()
	interpreter := writeReplayRuntime(t, directory)
	output := filepath.Join(directory, "generated.o")
	var stderr bytes.Buffer
	err := runScript(scriptRunOptions{
		interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
		scriptContent: "#!/bin/sh\nprintf original >" + shellQuote(output) + "\nmake expected\n", replays: []scriptReplayManifest{{
			Name: "make", Invocations: []scriptReplayInvocation{{Arguments: []string{"expected"}, Outputs: []string{output}}},
		}},
		tools: map[string]string{}, toolContracts: map[string]toolaction.Contract{},
		stdout: ioDiscard{}, stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("runScript() rejected an output created at the replay boundary: %v\nstderr: %s", err, stderr.String())
	}
}

func TestRunScriptReplayBrokerEstablishesNewOutputReceiptAtomically(t *testing.T) {
	runtimeRoot := t.TempDir()
	output := filepath.Join(runtimeRoot, "generated.o")
	manifest := scriptReplayManifest{
		Name: "make", Invocations: []scriptReplayInvocation{{Arguments: []string{"expected"}, Outputs: []string{output}}},
	}
	prepared, err := prepareScriptReplayRuntime(manifest)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := startScriptReplayBroker([]*scriptReplayRuntimeManifest{prepared})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close() }()
	if err := os.WriteFile(output, []byte("materialized"), 0o600); err != nil {
		t.Fatal(err)
	}
	const invocations = 16
	start := make(chan struct{})
	errors := make(chan error, invocations)
	var wait sync.WaitGroup
	for range invocations {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errors <- runScriptReplayProxy(broker.endpoint, 0, []string{"expected"})
		}()
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("runScriptReplayProxy() failed: %v", err)
		}
	}
	if err := verifyScriptReplayReceipts([]*scriptReplayRuntimeManifest{prepared}); err != nil {
		t.Fatalf("verifyScriptReplayReceipts() failed: %v", err)
	}
	if err := os.WriteFile(output, []byte("modified----"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runScriptReplayProxy(broker.endpoint, 0, []string{"expected"}); err == nil || !strings.Contains(err.Error(), "changed content after replay") {
		t.Fatalf("repeated runScriptReplayProxy() error = %v, want changed-content error", err)
	}
}

func TestScriptReplayBrokerCloseDrainsAcceptedRequest(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "generated.o")
	if err := os.WriteFile(output, []byte("materialized"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareScriptReplayRuntime(scriptReplayManifest{
		Name: "make", Invocations: []scriptReplayInvocation{{Arguments: []string{"expected"}, Outputs: []string{output}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := startScriptReplayBroker([]*scriptReplayRuntimeManifest{prepared})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Net: "unix", Name: broker.endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		broker.mu.Lock()
		accepted := len(broker.connections) == 1
		broker.mu.Unlock()
		if accepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("broker did not accept test connection")
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan error, 1)
	go func() { closed <- broker.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("broker close did not drain accepted request: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := writeScriptReplayRequest(connection, 0, []string{"expected"}); err != nil {
		t.Fatal(err)
	}
	if err := connection.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	exitCode, diagnostic, err := readScriptReplayResponse(connection)
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 || diagnostic != "" {
		t.Fatalf("drained replay response = (%d, %q), want success", exitCode, diagnostic)
	}
	if err := <-closed; err != nil {
		t.Fatalf("broker close failed after draining request: %v", err)
	}
}

func TestSnapshotScriptReplayOutputPinsRegularFileAndRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "output")
	original := filepath.Join(directory, "original")
	replacement := filepath.Join(directory, "replacement")
	if err := os.WriteFile(path, []byte("original"), 0o101); err != nil {
		t.Fatal(err)
	}
	fd, pinned, available, err := pinScriptReplayOutput(path)
	if err != nil || !available {
		t.Fatalf("pinScriptReplayOutput() = (%d, %#v, %t, %v), want regular file", fd, pinned, available, err)
	}
	defer syscall.Close(fd)
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	currentFD, current, available, err := pinScriptReplayOutput(path)
	if err != nil || !available {
		t.Fatalf("pin replacement = (%d, %#v, %t, %v)", currentFD, current, available, err)
	}
	_ = syscall.Close(currentFD)
	if sameScriptReplayFile(pinned, current) {
		t.Fatal("O_PATH descriptor followed a pathname replacement")
	}
	if err := os.Rename(path, replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original, path); err != nil {
		t.Fatal(err)
	}
	if _, available, err := snapshotScriptReplayOutput(path); err != nil || available {
		t.Fatalf("snapshot symlink = (available %t, error %v), want rejected non-regular output", available, err)
	}
	if info, err := os.Stat(original); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o101 {
		t.Fatalf("pinned unreadable output mode = %#o, want restored 0101", info.Mode().Perm())
	}
}

func TestStableScriptReplayFileRejectsConcurrentWriteMetadata(t *testing.T) {
	stable := syscall.Stat_t{
		Dev: 3, Ino: 7, Mode: syscall.S_IFREG | 0o600, Size: 11,
		Mtim: syscall.Timespec{Sec: 13, Nsec: 17},
		Ctim: syscall.Timespec{Sec: 19, Nsec: 23},
	}
	if !stableScriptReplayFile(stable, stable) {
		t.Fatal("identical regular-file metadata was unstable")
	}
	for _, mutate := range []func(*syscall.Stat_t){
		func(stat *syscall.Stat_t) { stat.Size++ },
		func(stat *syscall.Stat_t) { stat.Mtim.Nsec++ },
		func(stat *syscall.Stat_t) { stat.Ctim.Nsec++ },
		func(stat *syscall.Stat_t) { stat.Mode |= 0o100 },
		func(stat *syscall.Stat_t) { stat.Ino++ },
	} {
		changed := stable
		mutate(&changed)
		if stableScriptReplayFile(stable, changed) {
			t.Fatalf("changed metadata %#v was accepted as stable against %#v", changed, stable)
		}
	}
}

func TestRunScriptReplayProxyRejectsNewOutputMutationAfterReplay(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutation   func(string) string
		diagnostic string
	}{
		{
			name:       "content",
			mutation:   func(output string) string { return "printf modified >" + shellQuote(output) },
			diagnostic: "changed content after replay",
		},
		{
			name:       "executable bits",
			mutation:   func(output string) string { return "/bin/chmod 700 " + shellQuote(output) },
			diagnostic: "changed executable bits after replay",
		},
		{
			name:       "missing",
			mutation:   func(output string) string { return "/bin/rm " + shellQuote(output) },
			diagnostic: "missing regular output",
		},
		{
			name: "nonregular",
			mutation: func(output string) string {
				return "/bin/rm " + shellQuote(output) + " && /bin/mkdir " + shellQuote(output)
			},
			diagnostic: "missing regular output",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			interpreter := writeReplayRuntime(t, directory)
			output := filepath.Join(directory, "generated.o")
			err := runScript(scriptRunOptions{
				interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
				scriptContent: "#!/bin/sh\nprintf original >" + shellQuote(output) + "\nmake expected\n" + test.mutation(output) + "\n", replays: []scriptReplayManifest{{
					Name: "make", Invocations: []scriptReplayInvocation{{Arguments: []string{"expected"}, Outputs: []string{output}}},
				}},
				tools: map[string]string{}, toolContracts: map[string]toolaction.Contract{},
				stdout: ioDiscard{}, stderr: ioDiscard{},
			})
			if err == nil {
				t.Fatal("runScript() accepted a newly created replay output mutated after the boundary")
			}
			if !strings.Contains(err.Error(), test.diagnostic) {
				t.Fatalf("runScript() error = %q, want %q", err, test.diagnostic)
			}
		})
	}
}

func TestRunScriptReplayProxyRejectsUndeclaredArguments(t *testing.T) {
	directory := t.TempDir()
	interpreter := writeReplayRuntime(t, directory)
	output := filepath.Join(directory, "generated.o")
	if err := os.WriteFile(output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	err := runScript(scriptRunOptions{
		interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
		scriptContent: "#!/bin/sh\nmake unexpected\n", replays: []scriptReplayManifest{{
			Name: "make", Invocations: []scriptReplayInvocation{{Arguments: []string{"expected"}, Outputs: []string{output}}},
		}},
		tools: map[string]string{}, toolContracts: map[string]toolaction.Contract{},
		stdout: ioDiscard{}, stderr: &stderr,
	})
	if err == nil {
		t.Fatal("runScript() accepted undeclared replay arguments")
	}
	if !strings.Contains(err.Error(), "exit status 64") {
		t.Fatalf("runScript() error = %q, want replay exit status 64", err)
	}
	if !strings.Contains(stderr.String(), "rejected undeclared arguments") {
		t.Fatalf("stderr = %q, want undeclared-arguments diagnostic", stderr.String())
	}
}

func TestRunScriptReplayProxyRejectsMissingOrNonRegularOutput(t *testing.T) {
	directory := t.TempDir()
	interpreter := writeReplayRuntime(t, directory)
	for _, test := range []struct {
		name            string
		createDirectory bool
	}{
		{name: "missing"},
		{name: "directory", createDirectory: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(directory, test.name)
			if test.createDirectory {
				if err := os.Mkdir(output, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var stderr bytes.Buffer
			err := runScript(scriptRunOptions{
				interpreter: interpreter, interpreterArgs: []string{"sh"}, multicall: interpreter,
				scriptContent: "#!/bin/sh\nmake expected\n", replays: []scriptReplayManifest{{
					Name: "make", Invocations: []scriptReplayInvocation{{Arguments: []string{"expected"}, Outputs: []string{output}}},
				}},
				tools: map[string]string{}, toolContracts: map[string]toolaction.Contract{},
				stdout: ioDiscard{}, stderr: &stderr,
			})
			if err == nil {
				t.Fatal("runScript() accepted a missing or non-regular replay output")
			}
			if !strings.Contains(err.Error(), "exit status 66") {
				t.Fatalf("runScript() error = %q, want replay exit status 66", err)
			}
			if !strings.Contains(stderr.String(), "missing regular output") {
				t.Fatalf("stderr = %q, want regular-output diagnostic", stderr.String())
			}
		})
	}
}

func TestDecodeReplayManifestsRejectsMalformedInput(t *testing.T) {
	encode := func(value string) string {
		return base64.StdEncoding.EncodeToString([]byte(value))
	}
	for _, test := range []struct {
		name   string
		values []string
	}{
		{name: "base64", values: []string{"%%%"}},
		{name: "unknown field", values: []string{encode(`{"name":"make","invocations":[],"extra":true}`)}},
		{name: "invalid name", values: []string{encode(`{"name":"../make","invocations":[{"arguments":[],"outputs":[]}]}`)}},
		{name: "NUL", values: []string{encode(`{"name":"make","invocations":[{"arguments":["\u0000"],"outputs":[]}]}`)}},
		{name: "duplicate name", values: []string{
			encode(`{"name":"make","invocations":[{"arguments":["one"],"outputs":[]}]}`),
			encode(`{"name":"make","invocations":[{"arguments":["two"],"outputs":[]}]}`),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeReplayManifests(test.values); err == nil {
				t.Fatalf("decodeReplayManifests(%q) succeeded", test.values)
			}
		})
	}
}

func TestValidateReplayManifestsRejectsExternalToolCollision(t *testing.T) {
	err := validateReplayManifests([]scriptReplayManifest{{
		Name: "make", Invocations: []scriptReplayInvocation{{}},
	}}, map[string]string{"make": "/declared/make"})
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("validateReplayManifests() error = %v, want collision", err)
	}
}

// A configured runtime role must beat a same-named multicall fallback without
// bypassing explicit action contracts or importing ambient PATH entries.
func TestRunScriptConfiguredRuntimePrecedesMulticallFallback(t *testing.T) {
	for _, test := range []struct {
		name                         string
		configured, applet, explicit bool
		want                         string
	}{
		{name: "fallback", want: "multicall:argument\n"},
		{name: "configured", configured: true, want: "configured:argument\n"},
		{name: "applet", configured: true, applet: true, want: "applet:argument\n"},
		{name: "explicit", configured: true, explicit: true, want: "explicit:prefix:argument:from-contract\n"},
		{name: "explicit_over_applet", configured: true, applet: true, explicit: true, want: "explicit:prefix:argument:from-contract\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			runtime := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then printf '%s\n' selected sh; exit 0; fi
if [ "${0##*/}" = selected ]; then printf 'multicall:%s\n' "$1"; exit 0; fi
if [ "$1" = sh ]; then shift; exec /bin/sh "$@"; fi
exit 64
`)
			// Use the actual runner's validated directory constructor. Its
			// noncolliding configured command must remain available too.
			runtimeTools := map[string]string{
				"runtime-only": writeExecutable(t, directory, "runtime-only", "#!/bin/sh\nprintf 'runtime-only\\n'\n"),
			}
			if test.configured {
				runtimeTools["selected"] = writeExecutable(t, directory, "configured", "#!/bin/sh\nprintf 'configured:%s\\n' \"$1\"\n")
			}
			runtimePath, cleanup, err := toolaction.PrepareRuntimeToolDirectory(directory, runtimeTools)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			options := scriptRunOptions{
				interpreter: runtime, interpreterArgs: []string{"sh"}, multicall: runtime,
				scriptContent:   "selected argument\nruntime-only\n",
				runtimeToolPath: runtimePath,
				tools:           map[string]string{}, applets: map[string]string{},
				toolContracts:   map[string]toolaction.Contract{},
				requiredApplets: []string{"selected"},
			}
			if test.applet {
				options.applets["selected"] = writeExecutable(t, directory, "applet", "#!/bin/sh\nprintf 'applet:%s\\n' \"$1\"\n")
				options.toolContracts[scriptAppletRolePrefix+"selected"] = toolaction.Contract{
					Arguments: []string{}, Environment: map[string]string{},
				}
			}
			if test.explicit {
				options.tools["selected"] = writeExecutable(t, directory, "explicit", "#!/bin/sh\nprintf 'explicit:%s:%s:%s\\n' \"$1\" \"$2\" \"$SELECTED_MODE\"\n")
				options.toolContracts["selected"] = toolaction.Contract{
					Arguments:   []string{"prefix", toolaction.KbuildArgumentsSentinel},
					Environment: map[string]string{"SELECTED_MODE": "from-contract"},
				}
			}
			var stdout, stderr bytes.Buffer
			options.stdout, options.stderr = &stdout, &stderr
			if err := runScript(options); err != nil {
				t.Fatalf("runScript(): %v\nstderr: %s", err, stderr.String())
			}
			if got, want := stdout.String(), test.want+"runtime-only\n"; got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
		})
	}
}

func TestRunScriptRejectsInvalidConfiguredMulticallCollision(t *testing.T) {
	for _, kind := range []string{"directory", "nonexecutable", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			runtime := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then printf '%s\n' selected sh; exit 0; fi
if [ "$1" = sh ]; then shift; exec /bin/sh "$@"; fi
exit 64
`)
			runtimePath := filepath.Join(directory, "runtime-tools")
			if err := os.Mkdir(runtimePath, 0o700); err != nil {
				t.Fatal(err)
			}
			collision := filepath.Join(runtimePath, "selected")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(collision, 0o700)
			case "nonexecutable":
				err = os.WriteFile(collision, []byte("not executable"), 0o600)
			case "dangling":
				err = os.Symlink(filepath.Join(directory, "missing"), collision)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = runScript(scriptRunOptions{
				interpreter: runtime, interpreterArgs: []string{"sh"}, multicall: runtime,
				scriptContent: "printf 'must not run\\n'\n", runtimeToolPath: runtimePath,
				stdout: ioDiscard{}, stderr: ioDiscard{},
			})
			if err == nil || !strings.Contains(err.Error(), "configured runtime tool selected") {
				t.Fatalf("runScript() = %v, want invalid configured collision error", err)
			}
		})
	}
}
