package toolaction

import (
	"os"
	"path/filepath"
	"slices"
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
		`{"cc":{"arguments":[],"environment":{}}} {}`,
	} {
		if _, err := Decode(value); err == nil {
			t.Errorf("Decode(%q) succeeded", value)
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
