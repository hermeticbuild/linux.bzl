package toolaction

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestContractRoundTrip(t *testing.T) {
	contracts := map[string]Contract{
		"cc": {
			Arguments:   []string{"wrapper", KbuildArgumentsSentinel, "suffix"},
			Environment: map[string]string{"EXACT_ENV": "selected"},
		},
	}
	encoded, err := Encode(contracts)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"wrapper", KbuildArgumentsSentinel, "suffix"}
	if got := decoded["cc"].Arguments; !slices.Equal(got, want) {
		t.Fatalf("decoded arguments=%q, want %q", got, want)
	}
}

func TestContractRejectsMalformedData(t *testing.T) {
	for _, value := range []string{
		`{"CC":{"arguments":[],"environment":{}}}`,
		`{"cc":{"arguments":["missing-marker"],"environment":{}}}`,
		`{"cc":{"arguments":null,"environment":{}}}`,
		`{"cc":{"arguments":[],"environment":{"BAD-NAME":"value"}}}`,
		`{"cc":{"arguments":[],"environment":{"linux_bzl_default_directory_argument_0":"overwrite"}}}`,
		`{"cc":{"arguments":["__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__--sysroot=/anchor","__LINUX_BZL_KBUILD_ARGS_V1__","__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__--resource=/anchor"],"environment":{}}}`,
		`{"cc":{"arguments":["__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__sysroot=/anchor","__LINUX_BZL_KBUILD_ARGS_V1__"],"environment":{}}}`,
		`{"cc":{"arguments":["__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__--=/anchor","__LINUX_BZL_KBUILD_ARGS_V1__"],"environment":{}}}`,
		`{"cc":{"arguments":["__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__--sysroot","__LINUX_BZL_KBUILD_ARGS_V1__"],"environment":{}}}`,
		`{"cc":{"arguments":["__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__--sysroot=","__LINUX_BZL_KBUILD_ARGS_V1__"],"environment":{}}}`,
		`{"cc":{"arguments":["__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__--sysroot=/one","__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__--sysroot=/two","__LINUX_BZL_KBUILD_ARGS_V1__"],"environment":{}}}`,
		`{"cc":{"arguments":["__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__--sysroot=/anchor","__LINUX_BZL_KBUILD_ARGS_V1__","--sysroot=/configured"],"environment":{}}}`,
		`{"cc":{"arguments":[],"environment":{}}} {}`,
	} {
		if _, err := Decode(value); err == nil {
			t.Errorf("Decode(%q) succeeded", value)
		}
	}
}

func TestSpliceArgumentsAppliesConditionalDirectoryDefault(t *testing.T) {
	anchor := filepath.Join(t.TempDir(), "rust.sysroot")
	action := []string{
		DefaultDirectoryArgumentMarker + "--sysroot=" + anchor,
		KbuildArgumentsSentinel,
		"-Zunstable-options",
	}
	defaultSysroot := "--sysroot=" + filepath.Dir(anchor)
	for _, test := range []struct {
		name       string
		invocation []string
		want       []string
	}{
		{
			name:       "default",
			invocation: []string{"--crate-name", "macros"},
			want:       []string{defaultSysroot, "--crate-name", "macros", "-Zunstable-options"},
		},
		{
			name:       "source attached override",
			invocation: []string{"--sysroot=/dev/null", "--crate-name", "kernel"},
			want:       []string{"--sysroot=/dev/null", "--crate-name", "kernel", "-Zunstable-options"},
		},
		{
			name:       "source separate override",
			invocation: []string{"--sysroot", "/dev/null", "--crate-name", "kernel"},
			want:       []string{"--sysroot", "/dev/null", "--crate-name", "kernel", "-Zunstable-options"},
		},
		{
			name:       "near match does not override",
			invocation: []string{"--sysroot-extra=/different", "--crate-name", "macros"},
			want:       []string{defaultSysroot, "--sysroot-extra=/different", "--crate-name", "macros", "-Zunstable-options"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := SpliceArguments(action, test.invocation)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("SpliceArguments() = %q, want %q", got, test.want)
			}
		})
	}

	invalid := append([]string(nil), action...)
	invalid[0] = DefaultDirectoryArgumentMarker + "--sysroot=relative/anchor"
	for _, invocation := range [][]string{nil, {"--sysroot=/dev/null"}} {
		if _, err := SpliceArguments(invalid, invocation); err == nil {
			t.Fatalf("relative default directory anchor accepted with invocation %q", invocation)
		}
	}
}

func TestValidRoleUsesCanonicalManifestGrammar(t *testing.T) {
	for _, role := range []string{"cc", "objcopy-2", "script.runtime", "rustc_or_clippy"} {
		if !ValidRole(role) {
			t.Errorf("ValidRole(%q)=false", role)
		}
	}
	for _, role := range []string{"", "CC", "2cc", "cc/path", "cc role"} {
		if ValidRole(role) {
			t.Errorf("ValidRole(%q)=true", role)
		}
	}
}

func TestInvocationContractRoleUsesSemanticDriverMode(t *testing.T) {
	tests := []struct {
		name, role string
		arguments  []string
		want       string
	}{
		{name: "C link", role: "cc", arguments: []string{"first.o", "-o", "host-tool"}, want: "cc-link"},
		{name: "C++ attached output", role: "cxx", arguments: []string{"first.o", "-ohost-tool"}, want: "cxx-link"},
		{name: "compile", role: "cc", arguments: []string{"-c", "source.c", "-o", "source.o"}, want: "cc"},
		{name: "assemble", role: "cc", arguments: []string{"-S", "source.c", "-o", "source.s"}, want: "cc"},
		{name: "preprocess", role: "cc", arguments: []string{"-E", "source.c", "-o", "source.i"}, want: "cc"},
		{name: "dependencies", role: "cc", arguments: []string{"-M", "source.c", "-o", "source.d"}, want: "cc"},
		{name: "dependencies without system headers", role: "cc", arguments: []string{"-MM", "source.c", "-o", "source.d"}, want: "cc"},
		{name: "syntax only", role: "cc", arguments: []string{"-fsyntax-only", "source.c", "-o", "unused"}, want: "cc"},
		{name: "query with redirected plan output", role: "cc", arguments: []string{"--version"}, want: "cc"},
		{name: "missing output value", role: "cc", arguments: []string{"first.o", "-o"}, want: "cc"},
		{name: "non-driver", role: "ld", arguments: []string{"first.o", "-o", "host-tool"}, want: "ld"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := InvocationContractRole(test.role, test.arguments); got != test.want {
				t.Fatalf("InvocationContractRole(%q, %q) = %q, want %q", test.role, test.arguments, got, test.want)
			}
		})
	}
}

func TestCompilerInvocationProducesBinaryOutputUsesPrimaryDriverMode(t *testing.T) {
	for _, test := range []struct {
		name      string
		role      string
		arguments []string
		want      bool
	}{
		{name: "object", role: "cc", arguments: []string{"-c", "source.c", "-o", "source.o"}, want: true},
		{name: "object with side dependency", role: "cc", arguments: []string{"-c", "-MMD", "-MF", "source.d", "-osource.o", "source.c"}, want: true},
		{name: "link", role: "cxx", arguments: []string{"first.o", "-o", "tool"}, want: true},
		{name: "preprocess", role: "cc", arguments: []string{"-E", "source.c", "-o", "source.i"}},
		{name: "assembly text", role: "cc", arguments: []string{"-S", "source.c", "-o", "source.s"}},
		{name: "dependency only", role: "cc", arguments: []string{"-M", "source.c", "-o", "source.d"}},
		{name: "syntax only", role: "cc", arguments: []string{"-fsyntax-only", "source.c", "-o", "unused"}},
		{name: "non compiler", role: "ld", arguments: []string{"first.o", "-o", "image"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := CompilerInvocationProducesBinaryOutput(test.role, test.arguments); got != test.want {
				t.Fatalf("CompilerInvocationProducesBinaryOutput(%q, %q) = %t, want %t", test.role, test.arguments, got, test.want)
			}
		})
	}
}

func TestExpandExecutionRootValue(t *testing.T) {
	executionRoot := filepath.Join(string(filepath.Separator), "execroot")
	got, err := ExpandExecutionRootValue(
		"-I"+ExecutionRootMarker+"/external/toolchain/include:"+ExecutionRootMarker+"/bazel-out/cfg/bin/tool",
		executionRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "-I" + filepath.ToSlash(filepath.Join(executionRoot, "external/toolchain/include")) + ":" + filepath.ToSlash(filepath.Join(executionRoot, "bazel-out/cfg/bin/tool"))
	if got != want {
		t.Fatalf("expanded value = %q, want %q", got, want)
	}
	for _, test := range []struct {
		name, value, root string
	}{
		{name: "relative root", value: ExecutionRootMarker + "/tool", root: "relative"},
		{name: "bare marker", value: ExecutionRootMarker, root: executionRoot},
		{name: "malformed marker", value: "-I" + ExecutionRootMarker + "relative", root: executionRoot},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ExpandExecutionRootValue(test.value, test.root); err == nil {
				t.Fatalf("ExpandExecutionRootValue(%q, %q) succeeded", test.value, test.root)
			}
		})
	}
}

func TestPrepareRuntimeToolDirectoryUsesOnlyDeclaredRoles(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "selected-linker")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	directory, cleanup, err := PrepareRuntimeToolDirectory(root, map[string]string{"ld": executable})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	resolved, err := filepath.EvalSymlinks(filepath.Join(directory, "ld"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != executable {
		t.Fatalf("runtime ld = %q, want %q", resolved, executable)
	}
}

func TestInstallToolActionProxyRejectsInvalidShebangRuntime(t *testing.T) {
	contract := Contract{
		Arguments:   []string{KbuildArgumentsSentinel},
		Environment: map[string]string{},
	}
	for name, runtime := range map[string]string{
		"relative":      "script-runtime",
		"space in path": filepath.Join(string(filepath.Separator), "runtime path", "script-runtime"),
		"tab in path":   filepath.Join(string(filepath.Separator), "runtime\tpath", "script-runtime"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := InstallToolActionProxy(t.TempDir(), runtime, "cc", "/toolchain/cc", contract, nil); err == nil {
				t.Fatalf("InstallToolActionProxy with runtime %q succeeded", runtime)
			}
		})
	}
}

func TestInstallToolActionProxyMatchesInvocationContractRole(t *testing.T) {
	root := t.TempDir()
	multicall := filepath.Join(root, "multicall")
	if err := os.WriteFile(multicall, []byte(`#!/bin/sh
if [ "$1" != sh ]; then exit 90; fi
shift
exec /bin/sh "$@"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(root, "tool")
	if err := os.WriteFile(tool, []byte(`#!/bin/sh
printf '%s' "$SELECTED_MODE" > "$RESULT"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	proxyDirectory := filepath.Join(root, "proxies")
	if err := os.Mkdir(proxyDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	compile := Contract{
		Arguments:   []string{KbuildArgumentsSentinel},
		Environment: map[string]string{"SELECTED_MODE": "cc"},
	}
	link := Contract{
		Arguments:   []string{KbuildArgumentsSentinel},
		Environment: map[string]string{"SELECTED_MODE": "cc-link"},
	}
	proxy, err := InstallToolActionProxy(proxyDirectory, multicall, "cc", tool, compile, &link)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		arguments []string
	}{
		{name: "link", arguments: []string{"first.o", "-o", "tool"}},
		{name: "empty output", arguments: []string{"first.o", "-o", ""}},
		{name: "missing output", arguments: []string{"first.o", "-o"}},
		{name: "compile", arguments: []string{"-c", "source.c", "-o", "source.o"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := filepath.Join(root, "result-"+test.name)
			command := exec.Command(proxy, test.arguments...)
			command.Env = append(os.Environ(), "RESULT="+result)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("run proxy: %v\n%s", err, output)
			}
			got, err := os.ReadFile(result)
			if err != nil {
				t.Fatal(err)
			}
			if want := InvocationContractRole("cc", test.arguments); string(got) != want {
				t.Fatalf("proxy selected %q, want canonical contract role %q", got, want)
			}
		})
	}
}

func TestInstallToolActionProxyAppliesConditionalDirectoryDefault(t *testing.T) {
	root := t.TempDir()
	multicall := filepath.Join(root, "multicall")
	if err := os.WriteFile(multicall, []byte(`#!/bin/sh
if [ "$1" != sh ]; then exit 90; fi
shift
exec /bin/sh "$@"
`), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(root, "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$RESULT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	anchor := filepath.Join(root, "generated sysroot's", "rust.sysroot")
	if err := os.MkdirAll(filepath.Dir(anchor), 0o755); err != nil {
		t.Fatal(err)
	}
	contract := Contract{
		Arguments: []string{
			DefaultDirectoryArgumentMarker + "--sysroot=" + anchor,
			KbuildArgumentsSentinel,
		},
		Environment: map[string]string{},
	}
	proxy, err := InstallToolActionProxy(root, multicall, "rustc", tool, contract, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		arguments []string
		want      []string
	}{
		{
			name:      "default",
			arguments: []string{"--crate-name", "macros"},
			want:      []string{"--sysroot=" + filepath.Dir(anchor), "--crate-name", "macros"},
		},
		{
			name:      "override",
			arguments: []string{"--sysroot=/dev/null", "--crate-name", "kernel"},
			want:      []string{"--sysroot=/dev/null", "--crate-name", "kernel"},
		},
		{
			name:      "split override",
			arguments: []string{"--sysroot", "/dev/null", "--crate-name", "kernel"},
			want:      []string{"--sysroot", "/dev/null", "--crate-name", "kernel"},
		},
		{
			name:      "near match",
			arguments: []string{"--sysroot-extra=/different", "--crate-name", "macros"},
			want:      []string{"--sysroot=" + filepath.Dir(anchor), "--sysroot-extra=/different", "--crate-name", "macros"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := filepath.Join(root, "result-"+test.name)
			command := exec.Command(proxy, test.arguments...)
			command.Env = append(os.Environ(), "RESULT="+result)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("run proxy: %v\n%s", err, output)
			}
			data, err := os.ReadFile(result)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if !slices.Equal(got, test.want) {
				t.Fatalf("proxy arguments = %q, want %q", got, test.want)
			}
		})
	}
}
