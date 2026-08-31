package toolaction

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
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

func TestExecutionRootProvenancePathIsScopedSelfDelimitedAndCanonical(t *testing.T) {
	token, err := EncodeExecutionRootProvenancePath("host", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	value := "before=-I" + token + " after"
	got, err := RewriteExecutionRootProvenanceValue(value, func(scope, canonical string) (string, error) {
		if scope != "host" || canonical != "external/gcc/include" {
			t.Fatalf("resolver input = (%q, %q)", scope, canonical)
		}
		return "/physical/gcc/include", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "before=-I/physical/gcc/include after"; got != want {
		t.Fatalf("rewritten value = %q, want %q", got, want)
	}

	for _, test := range []struct {
		name, value string
	}{
		{name: "unknown scope", value: ExecutionRootProvenanceMarker + "build:external/gcc/include" + ExecutionRootProvenanceTerminator},
		{name: "parent traversal", value: ExecutionRootProvenanceMarker + "target:external/gcc/../../etc" + ExecutionRootProvenanceTerminator},
		{name: "dot component", value: ExecutionRootProvenanceMarker + "target:external/./gcc" + ExecutionRootProvenanceTerminator},
		{name: "unterminated", value: ExecutionRootProvenanceMarker + "target:external/gcc/include"},
		{name: "stray opener", value: "prefix\x07suffix"},
		{name: "stray terminator", value: "prefix\x08suffix"},
		{name: "slash suffix", value: token + "/../../etc"},
		{name: "backslash suffix", value: token + `\..\..\etc`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := RewriteExecutionRootProvenanceValue(test.value, func(_, _ string) (string, error) {
				return "/unused", nil
			}); err == nil {
				t.Fatalf("RewriteExecutionRootProvenanceValue(%q) succeeded", test.value)
			}
		})
	}
}

func TestExecutionRootProvenanceAuthorityRejectsAdjacentSuffixes(t *testing.T) {
	codec := fixedExecutionRootProvenanceCapabilityCodec(t, 0x42)
	core, err := EncodeExecutionRootProvenancePath("host", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	capability, err := codec.EncodePath("host", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name, suffix string
	}{
		{name: "dot extension", suffix: ".a"},
		{name: "star glob", suffix: "*"},
		{name: "question glob", suffix: "?"},
		{name: "lowercase", suffix: "a"},
		{name: "uppercase", suffix: "Z"},
		{name: "digit", suffix: "0"},
		{name: "underscore", suffix: "_suffix"},
		{name: "hyphen", suffix: "-suffix"},
		{name: "percent pattern", suffix: "%"},
		{name: "slash path", suffix: "/subdir"},
		{name: "backslash path", suffix: `\subdir`},
		{name: "quoted path continuation", suffix: `"/../sibling"`},
		{name: "single quoted path continuation", suffix: `'/../sibling'`},
		{name: "brace expansion", suffix: "{,.a}"},
		{name: "backtick", suffix: "`tail"},
		{name: "comma", suffix: ",tail"},
		{name: "semicolon", suffix: ";tail"},
		{name: "colon", suffix: ":tail"},
		{name: "pipe", suffix: "|tail"},
		{name: "ampersand", suffix: "&tail"},
		{name: "open parenthesis", suffix: "(tail"},
		{name: "close parenthesis", suffix: ")tail"},
		{name: "open bracket", suffix: "[tail"},
		{name: "close bracket", suffix: "]tail"},
		{name: "open angle", suffix: "<tail"},
		{name: "close angle", suffix: ">tail"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := RewriteExecutionRootProvenanceValue(core+test.suffix, func(_, _ string) (string, error) {
				return "/resolved/include", nil
			}); err == nil {
				t.Fatalf("RewriteExecutionRootProvenanceValue() = %q, want error", got)
			}
			if got, err := codec.NormalizeValue(capability + test.suffix); err == nil {
				t.Fatalf("NormalizeValue() = %q, want error", got)
			}
			got, err := CanonicalizeExecutionRootProvenanceCapabilityIdentity(capability + test.suffix)
			if err != nil {
				t.Fatalf("CanonicalizeExecutionRootProvenanceCapabilityIdentity() error = %v", err)
			}
			if want := core + test.suffix; got != want {
				t.Fatalf("CanonicalizeExecutionRootProvenanceCapabilityIdentity() = %q, want %q", got, want)
			}
		})
	}
}

func TestRewriteExecutionRootProvenancePreservesCapabilitySuffix(t *testing.T) {
	codec := fixedExecutionRootProvenanceCapabilityCodec(t, 0x42)
	core, err := EncodeExecutionRootProvenancePath("host", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	capability, err := codec.EncodePath("host", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeExecutionRootProvenancePath(capability); err == nil {
		t.Fatal("DecodeExecutionRootProvenancePath() accepted an authenticated planning capability as an exact runtime core")
	}
	got, err := RewriteExecutionRootProvenanceValue(capability+" ", func(_, _ string) (string, error) {
		return "/resolved/include", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "/resolved/include" + strings.TrimPrefix(capability, core) + " "; got != want {
		t.Fatalf("rewritten planning capability = %q, want preserved suffix in %q", got, want)
	}
}

func TestExecutionRootProvenanceAllowsWhitespaceAndPrefixes(t *testing.T) {
	codec := fixedExecutionRootProvenanceCapabilityCodec(t, 0x42)
	core, err := EncodeExecutionRootProvenancePath("host", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	capability, err := codec.EncodePath("host", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name, suffix string
	}{
		{name: "end"},
		{name: "space", suffix: " tail"},
		{name: "tab", suffix: "\ttail"},
		{name: "newline", suffix: "\ntail"},
		{name: "carriage return", suffix: "\rtail"},
		{name: "vertical tab", suffix: "\vtail"},
		{name: "form feed", suffix: "\ftail"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := RewriteExecutionRootProvenanceValue(core+test.suffix, func(_, _ string) (string, error) {
				return "/resolved/include", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if want := "/resolved/include" + test.suffix; got != want {
				t.Fatalf("RewriteExecutionRootProvenanceValue() = %q, want %q", got, want)
			}
			if err := ValidateExecutionRootProvenanceValue(capability + test.suffix); err != nil {
				t.Fatalf("ValidateExecutionRootProvenanceValue() error = %v", err)
			}
			got, err = codec.NormalizeValue(capability + test.suffix)
			if err != nil {
				t.Fatal(err)
			}
			if want := core + test.suffix; got != want {
				t.Fatalf("NormalizeValue() = %q, want %q", got, want)
			}
		})
	}

	for _, prefix := range []string{"-I", `prefix.a*?Az0_-%/\=`} {
		got, err := RewriteExecutionRootProvenanceValue(prefix+core, func(_, _ string) (string, error) {
			return "/resolved/include", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if want := prefix + "/resolved/include"; got != want {
			t.Fatalf("prefixed runtime token = %q, want %q", got, want)
		}
		got, err = codec.NormalizeValue(prefix + capability)
		if err != nil {
			t.Fatal(err)
		}
		if want := prefix + core; got != want {
			t.Fatalf("prefixed capability = %q, want %q", got, want)
		}
	}
}

func TestExecutionRootProvenanceCapabilityRoundTrip(t *testing.T) {
	codec := fixedExecutionRootProvenanceCapabilityCodec(t, 0x42)
	hostCapability, err := codec.EncodePath("host", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	targetCapability, err := codec.EncodePath("target", "bazel-out/toolchain/compiler")
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range []string{hostCapability, targetCapability} {
		if err := ValidateExecutionRootProvenanceValue(capability); err != nil {
			t.Fatalf("capability is not a syntactically valid provenance token: %v", err)
		}
	}
	hostRuntime, err := EncodeExecutionRootProvenancePath("host", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	targetRuntime, err := EncodeExecutionRootProvenancePath("target", "bazel-out/toolchain/compiler")
	if err != nil {
		t.Fatal(err)
	}

	value := "prefix=-I" + hostCapability + " between " + targetCapability + " suffix"
	got, err := codec.NormalizeValue(value)
	if err != nil {
		t.Fatal(err)
	}
	want := "prefix=-I" + hostRuntime + " between " + targetRuntime + " suffix"
	if got != want {
		t.Fatalf("NormalizeValue() = %q, want %q", got, want)
	}
	if strings.Contains(got, executionRootProvenanceCapabilitySuffix) {
		t.Fatalf("normalized value retained capability data: %q", got)
	}

	otherCodec := fixedExecutionRootProvenanceCapabilityCodec(t, 0x99)
	otherCapability, err := otherCodec.EncodePath("host", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	if otherCapability == hostCapability {
		t.Fatal("different workload keys produced the same capability")
	}
	otherNormalized, err := otherCodec.NormalizeValue(otherCapability)
	if err != nil {
		t.Fatal(err)
	}
	if otherNormalized != hostRuntime {
		t.Fatalf("normalization depends on ephemeral key: got %q, want %q", otherNormalized, hostRuntime)
	}

	hostIdentity, err := CanonicalizeExecutionRootProvenanceCapabilityIdentity(hostCapability)
	if err != nil {
		t.Fatal(err)
	}
	otherIdentity, err := CanonicalizeExecutionRootProvenanceCapabilityIdentity(otherCapability)
	if err != nil {
		t.Fatal(err)
	}
	if hostIdentity != hostRuntime || otherIdentity != hostRuntime || hostIdentity != otherIdentity {
		t.Fatalf("identity canonicalization retained ephemeral key data: first=%q second=%q want=%q", hostIdentity, otherIdentity, hostRuntime)
	}
	mixed := "cap=" + hostCapability + " raw=" + targetRuntime
	mixedIdentity, err := CanonicalizeExecutionRootProvenanceCapabilityIdentity(mixed)
	if err != nil {
		t.Fatal(err)
	}
	if want := "cap=" + hostRuntime + " raw=" + targetRuntime; mixedIdentity != want {
		t.Fatalf("mixed identity canonicalization = %q, want %q", mixedIdentity, want)
	}
}

func TestExecutionRootProvenanceCapabilityPreservesLexicalPathOrder(t *testing.T) {
	firstCodec := fixedExecutionRootProvenanceCapabilityCodec(t, 0x42)
	secondCodec := fixedExecutionRootProvenanceCapabilityCodec(t, 0x99)
	paths := []string{
		"external/gcc",
		"external/gcc/include",
		"external/llvm",
		"bazel-out/toolchain/compiler",
	}
	cores := make([]string, 0, len(paths))
	capabilities := make([]string, 0, len(paths))
	for i, path := range paths {
		core, err := EncodeExecutionRootProvenancePath("target", path)
		if err != nil {
			t.Fatal(err)
		}
		codec := firstCodec
		if i%2 != 0 {
			codec = secondCodec
		}
		capability, err := codec.EncodePath("target", path)
		if err != nil {
			t.Fatal(err)
		}
		cores = append(cores, core)
		capabilities = append(capabilities, capability)
	}
	for i := range cores {
		for j := range cores {
			if i == j {
				continue
			}
			coreOrder := strings.Compare(cores[i], cores[j])
			capabilityOrder := strings.Compare(capabilities[i], capabilities[j])
			if coreOrder != capabilityOrder {
				t.Fatalf("capability order for %q and %q = %d, deterministic core order = %d", paths[i], paths[j], capabilityOrder, coreOrder)
			}
		}
	}

	sortedCores := append([]string(nil), cores...)
	sortedCapabilities := append([]string(nil), capabilities...)
	sort.Strings(sortedCores)
	sort.Strings(sortedCapabilities)
	identitySorted := make([]string, 0, len(sortedCapabilities))
	for _, capability := range sortedCapabilities {
		canonical, err := CanonicalizeExecutionRootProvenanceCapabilityIdentity(capability)
		if err != nil {
			t.Fatal(err)
		}
		identitySorted = append(identitySorted, canonical)
	}
	if !slices.Equal(identitySorted, sortedCores) {
		t.Fatalf("sort-like capability order canonicalized to %q, want %q", identitySorted, sortedCores)
	}

	prefixCore, prefixCapability := cores[0], capabilities[0]
	descendantCore, descendantCapability := cores[1], capabilities[1]
	if strings.Compare(prefixCore, descendantCore) != strings.Compare(prefixCapability, descendantCapability) {
		t.Fatalf("prefix-related paths changed lexical order: core=(%q,%q) capability=(%q,%q)", prefixCore, descendantCore, prefixCapability, descendantCapability)
	}
}

func TestExecutionRootProvenanceCapabilityRejectsForgery(t *testing.T) {
	codec := fixedExecutionRootProvenanceCapabilityCodec(t, 0x42)
	capability, err := codec.EncodePath("host", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	runtimeToken, err := EncodeExecutionRootProvenancePath("host", "external/gcc/include")
	if err != nil {
		t.Fatal(err)
	}
	tagOffset := len(runtimeToken) + len(executionRootProvenanceCapabilitySuffix)
	mutatedTag := capability[:tagOffset] + differentHexByte(capability[tagOffset]) + capability[tagOffset+1:]
	suffixOnly := strings.TrimPrefix(capability, runtimeToken)
	for _, test := range []struct {
		name  string
		codec *ExecutionRootProvenanceCapabilityCodec
		value string
	}{
		{name: "wrong workload key", codec: fixedExecutionRootProvenanceCapabilityCodec(t, 0x99), value: capability},
		{name: "mutated scope", codec: codec, value: strings.Replace(capability, ExecutionRootProvenanceMarker+"host:", ExecutionRootProvenanceMarker+"target:", 1)},
		{name: "mutated path", codec: codec, value: strings.Replace(capability, "external/gcc/include", "external/gcc/lib", 1)},
		{name: "mutated tag", codec: codec, value: mutatedTag},
		{name: "raw deterministic token", codec: codec, value: runtimeToken},
		{name: "core stripped", codec: codec, value: suffixOnly},
		{name: "suffix detached before core", codec: codec, value: suffixOnly + runtimeToken},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := test.codec.NormalizeValue("before" + test.value + " after"); err == nil {
				t.Fatalf("NormalizeValue() = %q, want error", got)
			}
		})
	}

	for _, malformedCapability := range []string{
		runtimeToken + executionRootProvenanceCapabilitySuffix,
		runtimeToken + executionRootProvenanceCapabilitySuffix + strings.Repeat("a", executionRootProvenanceCapabilityTagSize-1),
		runtimeToken + executionRootProvenanceCapabilitySuffix + strings.Repeat("A", executionRootProvenanceCapabilityTagSize),
		runtimeToken + executionRootProvenanceCapabilitySuffix + strings.Repeat("g", executionRootProvenanceCapabilityTagSize),
		executionRootProvenanceCapabilitySuffix + strings.Repeat("a", executionRootProvenanceCapabilityTagSize),
		"before" + executionRootProvenanceCapabilitySuffix + strings.Repeat("a", executionRootProvenanceCapabilityTagSize) + "after",
		capability + executionRootProvenanceCapabilitySuffix + strings.Repeat("a", executionRootProvenanceCapabilityTagSize),
	} {
		if got, err := codec.NormalizeValue(malformedCapability); err == nil {
			t.Fatalf("NormalizeValue() = %q, want error", got)
		}
		if got, err := CanonicalizeExecutionRootProvenanceCapabilityIdentity(malformedCapability); err == nil {
			t.Fatalf("CanonicalizeExecutionRootProvenanceCapabilityIdentity() = %q, want error", got)
		}
	}
}

func TestExecutionRootProvenanceCapabilityValidatesInputs(t *testing.T) {
	if _, err := newExecutionRootProvenanceCapabilityCodec(make([]byte, executionRootProvenanceCapabilityKeySize-1)); err == nil {
		t.Fatal("short capability key was accepted")
	}
	codec := fixedExecutionRootProvenanceCapabilityCodec(t, 0x42)
	for _, test := range []struct {
		name, scope, canonical string
	}{
		{name: "unknown scope", scope: "build", canonical: "external/gcc/include"},
		{name: "empty path", scope: "host", canonical: ""},
		{name: "parent path", scope: "target", canonical: "external/gcc/../include"},
		{name: "reserved delimiter", scope: "target", canonical: "external/gcc/\x07include"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := codec.EncodePath(test.scope, test.canonical); err == nil {
				t.Fatalf("EncodePath() = %q, want error", got)
			}
		})
	}
	var nilCodec *ExecutionRootProvenanceCapabilityCodec
	if _, err := nilCodec.EncodePath("host", "external/gcc/include"); err == nil {
		t.Fatal("nil codec encoded a capability")
	}
	if _, err := nilCodec.NormalizeValue("ordinary text"); err == nil {
		t.Fatal("nil codec normalized a value")
	}
	if got, err := codec.NormalizeValue("ordinary text without provenance tokens"); err != nil || got != "ordinary text without provenance tokens" {
		t.Fatalf("NormalizeValue(ordinary text) = %q, %v", got, err)
	}
}

func fixedExecutionRootProvenanceCapabilityCodec(t *testing.T, fill byte) *ExecutionRootProvenanceCapabilityCodec {
	t.Helper()
	key := make([]byte, executionRootProvenanceCapabilityKeySize)
	for i := range key {
		key[i] = fill
	}
	codec, err := newExecutionRootProvenanceCapabilityCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func differentHexByte(value byte) string {
	if value == '0' {
		return "1"
	}
	return "0"
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
