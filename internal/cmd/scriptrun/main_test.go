package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func writeExecutable(t *testing.T, directory, name, contents string) string {
	t.Helper()
	filename := filepath.Join(directory, name)
	if err := os.WriteFile(filename, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return filename
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
	directory := t.TempDir()
	interpreter := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then
	printf '%s\n' '[' sh
	exit 0
fi
if [ "$1" = sh ]; then
	shift
	exec /bin/sh "$@"
fi
exit 64
`)
	relativeOutput := "child/generated 'file'.o"
	absoluteOutput := filepath.Join(directory, "absolute output")
	if err := os.Mkdir(filepath.Join(directory, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{filepath.Join(directory, relativeOutput), absoluteOutput} {
		if err := os.WriteFile(output, []byte("materialized"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifestJSON, err := json.Marshal(scriptReplayManifest{
		Name: "make",
		Invocations: []scriptReplayInvocation{{
			Arguments: []string{"-f", "Makefile", "obj=init dir", "quoted'value"},
			Outputs:   []string{relativeOutput, absoluteOutput},
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
		scriptContent:   "#!/bin/sh\nmake -f Makefile 'obj=init dir' \"quoted'value\"\nprintf replay-ok\n",
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
}

func TestRunScriptReplayProxyRejectsUndeclaredArguments(t *testing.T) {
	directory := t.TempDir()
	interpreter := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then
	printf '%s\n' '[' sh
	exit 0
fi
shift
exec /bin/sh "$@"
`)
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
	if !strings.Contains(stderr.String(), "rejected undeclared arguments") {
		t.Fatalf("stderr = %q, want undeclared-arguments diagnostic", stderr.String())
	}
}

func TestRunScriptReplayProxyRejectsMissingOrNonRegularOutput(t *testing.T) {
	directory := t.TempDir()
	interpreter := writeExecutable(t, directory, "runtime", `#!/bin/sh
if [ "$1" = --list ]; then
	printf '%s\n' '[' sh
	exit 0
fi
shift
exec /bin/sh "$@"
`)
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
