package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestProjectProbeCandidateArgumentsCompilerPredefines(t *testing.T) {
	arguments := []string{
		"--target=x86_64-linux-gnu",
		"-nostdinc",
		"-I__LINUX_BZL_SOURCE_TREE__/include",
		"-include", "__LINUX_BZL_SOURCE_TREE__/include/linux/hidden.h",
		"-DSELECTED=drivers/one",
		"-D", "OTHER=drivers/two",
		"-DFUNCTION(x)=three",
		"-Wa,-gdwarf-5",
		"-Xassembler", "--fatal-warnings",
		"-Wl,--build-id",
		"-Xlinker", "--gc-sections",
		"-c",
		"-o", "drivers/example.o",
		"drivers/example.c",
		"-MMD",
		"-MFdrivers/example.d",
		"-Wp,-MMD,drivers/wp.d",
		"-O2",
	}
	original := slices.Clone(arguments)
	got, origins, err := ProjectProbeCandidateArguments(
		ProbeCandidateProjectionCompilerPredefines,
		arguments,
		[]string{"drivers/example.c"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--target=x86_64-linux-gnu",
		"-nostdinc",
		"-DSELECTED=1",
		"-D", "OTHER=1",
		"-DFUNCTION(x)=three",
		"-O2",
	}
	wantOrigins := []int{0, 1, 5, 6, 7, 8, 22}
	if !slices.Equal(got, want) || !slices.Equal(origins, wantOrigins) {
		t.Fatalf("compiler-predefine projection = %#v origins %#v, want %#v origins %#v", got, origins, want, wantOrigins)
	}
	if !slices.Equal(arguments, original) {
		t.Fatalf("compiler-predefine projection mutated caller argv: got %#v, want %#v", arguments, original)
	}
	if _, err := ValidateProbeCandidateArguments(ProbeCandidatePolicyCC, got); err != nil {
		t.Fatalf("projected compiler-predefine candidate is invalid: %v", err)
	}
}

func TestProjectProbeCandidateArgumentsCompilerPredefinesPreservesDefaultIncludeSuppression(t *testing.T) {
	arguments := []string{
		"-Ibefore", "-nostdinc", "-I-", "-nostdinc++",
		"-isystem", "source/include", "-nobuiltininc", "-nostdlibinc",
		"source.c", "-DKEEP=7",
	}
	original := slices.Clone(arguments)
	got, origins, err := ProjectProbeCandidateArguments(
		ProbeCandidateProjectionCompilerPredefines, arguments, []string{"source.c"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-nostdinc", "-nostdinc++", "-nobuiltininc", "-nostdlibinc", "-DKEEP=1"}
	wantOrigins := []int{1, 3, 6, 7, 9}
	if !slices.Equal(got, want) || !slices.Equal(origins, wantOrigins) {
		t.Fatalf("default include suppression projection = %#v origins %#v, want %#v origins %#v", got, origins, want, wantOrigins)
	}
	if !slices.Equal(arguments, original) {
		t.Fatal("default include suppression projection mutated caller argv")
	}
	if _, err := ValidateProbeCandidateArguments(ProbeCandidatePolicyCC, got); err != nil {
		t.Fatalf("projected default include suppression is invalid: %v", err)
	}
}

func TestProjectProbeCandidateArgumentsCompilerPredefinesRejectsUnmodeledFrontend(t *testing.T) {
	for _, test := range []struct {
		name string
		argv []string
		want string
	}{
		{name: "separated language", argv: []string{"-x", "c++"}, want: "language override"},
		{name: "joined language", argv: []string{"-xassembler-with-cpp"}, want: "language override"},
		{name: "preprocessor forwarding", argv: []string{"-Xpreprocessor", "-DOVERRIDE"}, want: "frontend forwarding"},
		{name: "Wp macro forwarding", argv: []string{"-Wp,-DOVERRIDE=1"}, want: "preprocessor forwarding"},
		{name: "Clang frontend", argv: []string{"-Xclang", "-fmodules"}, want: "frontend forwarding"},
		{name: "LLVM backend", argv: []string{"-mllvm", "-opaque-pass"}, want: "frontend forwarding"},
		{name: "precompiled header", argv: []string{"-include-pch", "kernel.pch"}, want: "precompiled or virtual include"},
		{name: "virtual overlay", argv: []string{"-ivfsoverlay", "overlay.yaml"}, want: "precompiled or virtual include"},
		{name: "missing include", argv: []string{"-include"}, want: "no operand"},
		{name: "option delimiter", argv: []string{"--", "source.c"}, want: "option terminator"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := ProjectProbeCandidateArguments(
				ProbeCandidateProjectionCompilerPredefines,
				test.argv,
				nil,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ProjectProbeCandidateArguments(%q) error = %v, want substring %q", test.argv, err, test.want)
			}
		})
	}
}

func TestProjectProbeCandidateArgumentsCompilerInitialStateOwnsCommentOutput(t *testing.T) {
	for _, test := range []struct {
		name            string
		arguments, want []string
		origins         []int
	}{
		{
			name:      "literal output modes",
			arguments: []string{"-C", "-P", "-CC", "-DKEEP=2", "source.lds.S"},
			want:      []string{"-P", "-DKEEP=1"},
			origins:   []int{1, 3},
		},
		{
			name:      "scalar payloads and macro bodies are not switches",
			arguments: []string{"-G", "-C", "-target", "-CC", "-meabi", "-C", "-U", "-CC", "-D", "F(x)=-C", "-DF(x)=-CC", "source.lds.S"},
			want:      []string{"-G", "-C", "-target", "-CC", "-meabi", "-C", "-U", "-CC", "-D", "F(x)=-C", "-DF(x)=-CC"},
			origins:   []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		},
		{
			name:      "forwarded nonpreprocessor switches",
			arguments: []string{"-Xassembler", "-C", "-Wl,-CC", "-Wa,-C", "source.lds.S"},
			want:      []string{},
			origins:   []int{},
		},
		{
			name:      "similar spellings are not output switches",
			arguments: []string{"-CFUTURE", "-CCfuture", "-DF(x)=-CC", "source.lds.S"},
			want:      []string{"-CFUTURE", "-CCfuture", "-DF(x)=-CC"},
			origins:   []int{0, 1, 2},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := slices.Clone(test.arguments)
			got, origins, err := ProjectProbeCandidateArguments(
				ProbeCandidateProjectionCompilerPredefines, test.arguments, []string{"source.lds.S"},
			)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) || !slices.Equal(origins, test.origins) {
				t.Fatalf("projected = %q at %v, want %q at %v", got, origins, test.want, test.origins)
			}
			if !slices.Equal(test.arguments, original) {
				t.Fatal("query projection mutated executable source arguments")
			}
			if _, err := ValidateProbeCandidateArguments(ProbeCandidatePolicyCC, got); err != nil {
				t.Fatalf("projected candidate failed validation: %v", err)
			}
		})
	}
}

func TestProjectProbeCandidateArgumentsCompilerPredefinesUsesExactTranslationUnitProvenance(t *testing.T) {
	arguments := []string{
		"--wrapper-mode", "mode.c",
		"-fmacro-prefix-map=/mapped/drivers/example.c=drivers/example.c",
		"/mapped/drivers/example.c",
		"-O2",
	}
	got, origins, err := ProjectProbeCandidateArguments(
		ProbeCandidateProjectionCompilerPredefines,
		arguments,
		[]string{"/mapped/drivers/example.c"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--wrapper-mode", "mode.c",
		"-fmacro-prefix-map=/mapped/drivers/example.c=drivers/example.c",
		"-O2",
	}
	wantOrigins := []int{0, 1, 2, 4}
	if !slices.Equal(got, want) || !slices.Equal(origins, wantOrigins) {
		t.Fatalf("compiler-predefine projection = %#v origins %#v, want %#v origins %#v", got, origins, want, wantOrigins)
	}
}

func TestProjectProbeCandidateArgumentsCompilerPredefinesRequiresExactTranslationUnitOnce(t *testing.T) {
	for _, test := range []struct {
		name    string
		argv    []string
		sources []string
	}{
		{name: "missing", argv: []string{"-O2"}, sources: []string{"source.c"}},
		{name: "duplicate argv", argv: []string{"source.c", "source.c"}, sources: []string{"source.c"}},
		{name: "duplicate declaration", argv: []string{"source.c"}, sources: []string{"source.c", "source.c"}},
		{name: "option spelling", argv: []string{"-source.c"}, sources: []string{"-source.c"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := ProjectProbeCandidateArguments(
				ProbeCandidateProjectionCompilerPredefines,
				test.argv,
				test.sources,
			); err == nil {
				t.Fatalf("ProjectProbeCandidateArguments(%q, %q) unexpectedly succeeded", test.argv, test.sources)
			}
		})
	}
}

func TestValidateProbeCandidateArgumentsAcceptsSemanticOptionsWithoutACompilerWhitelist(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		policy string
		argv   []string
	}{
		{name: "empty cc", policy: ProbeCandidatePolicyCC},
		{name: "empty ld", policy: ProbeCandidatePolicyLD},
		{name: "empty cc link", policy: ProbeCandidatePolicyCCLink},
		{name: "G zero", policy: ProbeCandidatePolicyCC, argv: []string{"-G", "0"}},
		{name: "Clang BPF target", policy: ProbeCandidatePolicyCC, argv: []string{"-target", "bpf"}},
		{name: "Clang ARM EABI", policy: ProbeCandidatePolicyCC, argv: []string{"-meabi", "gnu"}},
		{
			name: "ordinary ld options", policy: ProbeCandidatePolicyLD,
			argv: []string{"-m", "elf_x86_64", "-z", "noexecstack", "--eh-frame-hdr"},
		},
		{
			name: "unknown future scalar", policy: ProbeCandidatePolicyCC,
			argv: []string{"-future-compiler-capability", "opaque;not-a-shell"},
		},
		{
			name: "safe LLVM backend option", policy: ProbeCandidatePolicyCC,
			argv: []string{"-mllvm", "-future-kernel-pass=2"},
		},
		{name: "split DWARF in private scratch", policy: ProbeCandidatePolicyCC, argv: []string{"-gsplit-dwarf"}},
		{name: "macro prefix map", policy: ProbeCandidatePolicyCC, argv: []string{"-fmacro-prefix-map=__LINUX_BZL_SOURCE_TREE__/="}},
		{name: "file prefix map", policy: ProbeCandidatePolicyCC, argv: []string{"-ffile-prefix-map=/workspace/kernel=/usr/src/linux"}},
		{name: "debug prefix map", policy: ProbeCandidatePolicyCC, argv: []string{"-fdebug-prefix-map=old=new"}},
		{name: "disable sanitizer ignorelist", policy: ProbeCandidatePolicyCC, argv: []string{"-fno-sanitize-ignorelist"}},
		{name: "disable sanitizer blacklist", policy: ProbeCandidatePolicyCC, argv: []string{"-fno-sanitize-blacklist"}},
		{name: "safe linker forwarding", policy: ProbeCandidatePolicyCCLink, argv: []string{"-Wl,-z,now"}},
		{name: "safe assembler forwarding", policy: ProbeCandidatePolicyCC, argv: []string{"-Wa,-march=armv8-a"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			paths, err := ValidateProbeCandidateArguments(test.policy, test.argv)
			if err != nil {
				t.Fatalf("ValidateProbeCandidateArguments(%q, %q): %v", test.policy, test.argv, err)
			}
			if len(paths) != 0 {
				t.Fatalf("semantic argv returned path operands %#v", paths)
			}
		})
	}
}

func TestValidateProbeCandidateArgumentsIdentifiesTypedReadOnlyPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		argv   []string
		kinds  []ProbeCandidatePathKind
		values []string
	}{
		{
			name: "split include", argv: []string{"-I", "/execroot/linux/include"},
			kinds: []ProbeCandidatePathKind{ProbeCandidatePathInclude}, values: []string{"/execroot/linux/include"},
		},
		{
			name: "joined include", argv: []string{"-I/execroot/linux/include"},
			kinds: []ProbeCandidatePathKind{ProbeCandidatePathInclude}, values: []string{"/execroot/linux/include"},
		},
		{
			name: "equals system include", argv: []string{"-isystem=external/toolchain/include"},
			kinds: []ProbeCandidatePathKind{ProbeCandidatePathInclude}, values: []string{"external/toolchain/include"},
		},
		{
			name: "forced source include", argv: []string{"-include", "/execroot/linux/include/generated/autoconf.h"},
			kinds: []ProbeCandidatePathKind{ProbeCandidatePathForcedInclude}, values: []string{"/execroot/linux/include/generated/autoconf.h"},
		},
		{
			name: "sanitizer ignorelist", argv: []string{"-fsanitize-ignorelist=/dev/null"},
			kinds: []ProbeCandidatePathKind{ProbeCandidatePathRegularFile}, values: []string{"/dev/null"},
		},
		{
			name: "repeated sanitizer lists with comma-bearing filenames",
			argv: []string{
				"-fsanitize-ignorelist=/execroot/linux/ignore,first.scl",
				"-fsanitize-blacklist=/execroot/linux/ignore,second.scl",
			},
			kinds:  []ProbeCandidatePathKind{ProbeCandidatePathRegularFile, ProbeCandidatePathRegularFile},
			values: []string{"/execroot/linux/ignore,first.scl", "/execroot/linux/ignore,second.scl"},
		},
		{
			name: "randomized layout seed", argv: []string{"-frandomize-layout-seed-file=/dev/null"},
			kinds: []ProbeCandidatePathKind{ProbeCandidatePathRegularFile}, values: []string{"/dev/null"},
		},
		{
			name: "assembler forwarded includes", argv: []string{"-Wa,-I,/execroot/linux/arch/x86/include,-Iexternal/toolchain/asm"},
			kinds:  []ProbeCandidatePathKind{ProbeCandidatePathInclude, ProbeCandidatePathInclude},
			values: []string{"/execroot/linux/arch/x86/include", "external/toolchain/asm"},
		},
		{
			name: "preprocessor forwarded forced include", argv: []string{"-Wp,-include,external/toolchain/include/limits.h"},
			kinds: []ProbeCandidatePathKind{ProbeCandidatePathForcedInclude}, values: []string{"external/toolchain/include/limits.h"},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			paths, err := ValidateProbeCandidateArguments(ProbeCandidatePolicyCC, test.argv)
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) != len(test.values) {
				t.Fatalf("path operands = %#v, want %q", paths, test.values)
			}
			for index, path := range paths {
				if path.Argument < 0 || path.Argument >= len(test.argv) || path.Start < 0 || path.End > len(test.argv[path.Argument]) || path.Start >= path.End {
					t.Fatalf("path operand %d has invalid span %#v in %q", index, path, test.argv)
				}
				if got := test.argv[path.Argument][path.Start:path.End]; got != test.values[index] || path.Kind != test.kinds[index] {
					t.Fatalf("path operand %d = %q/%q, want %q/%q", index, path.Kind, got, test.kinds[index], test.values[index])
				}
			}
		})
	}
}

func TestValidateProbeCandidateArgumentsRejectsSecurityAndIOAuthority(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		policy string
		argv   []string
	}{
		{name: "unknown policy", policy: "gcc", argv: []string{"-Wall"}},
		{name: "empty word", policy: ProbeCandidatePolicyCC, argv: []string{""}},
		{name: "control byte", policy: ProbeCandidatePolicyCC, argv: []string{"-DVALUE=1\n-oout"}},
		{name: "response file", policy: ProbeCandidatePolicyCC, argv: []string{"@options.rsp"}},
		{name: "linker forwarded response", policy: ProbeCandidatePolicyCCLink, argv: []string{"-Wl,@options.rsp"}},
		{name: "assembler forwarded response", policy: ProbeCandidatePolicyCC, argv: []string{"-Wa,@options.rsp"}},
		{name: "preprocessor forwarded response", policy: ProbeCandidatePolicyCC, argv: []string{"-Wp,@options.rsp"}},
		{name: "positional input", policy: ProbeCandidatePolicyCC, argv: []string{"escape.c"}},
		{name: "path disguised as scalar", policy: ProbeCandidatePolicyCC, argv: []string{"-unknown", "../escape.c"}},
		{name: "unknown joined absolute path", policy: ProbeCandidatePolicyCC, argv: []string{"--future-path=/etc/passwd"}},
		{name: "missing define operand before managed boundary", policy: ProbeCandidatePolicyCC, argv: []string{"-D"}},
		{name: "stdin input", policy: ProbeCandidatePolicyCC, argv: []string{"-"}},
		{name: "option terminator", policy: ProbeCandidatePolicyCC, argv: []string{"--", "escape.c"}},
		{name: "split output", policy: ProbeCandidatePolicyCC, argv: []string{"-o", "escape.o"}},
		{name: "joined output", policy: ProbeCandidatePolicyCC, argv: []string{"-oescape.o"}},
		{name: "long output", policy: ProbeCandidatePolicyLD, argv: []string{"--output=escape.o"}},
		{name: "compile mode", policy: ProbeCandidatePolicyCC, argv: []string{"-c"}},
		{name: "joined language mode", policy: ProbeCandidatePolicyCC, argv: []string{"-xc"}},
		{name: "dependency mode", policy: ProbeCandidatePolicyCC, argv: []string{"-MMD"}},
		{name: "split dependency file", policy: ProbeCandidatePolicyCC, argv: []string{"-MF", "escape.d"}},
		{name: "joined dependency file", policy: ProbeCandidatePolicyCC, argv: []string{"-MFescape.d"}},
		{name: "long dependency file", policy: ProbeCandidatePolicyLD, argv: []string{"--dependency-file=escape.d"}},
		{name: "optimization report", policy: ProbeCandidatePolicyCC, argv: []string{"-fopt-info=report"}},
		{name: "split DWARF linker policy", policy: ProbeCandidatePolicyCCLink, argv: []string{"-gsplit-dwarf"}},
		{name: "split DWARF ld policy", policy: ProbeCandidatePolicyLD, argv: []string{"-gsplit-dwarf"}},
		{name: "split DWARF variant", policy: ProbeCandidatePolicyCC, argv: []string{"-gsplit-dwarf=split"}},
		{name: "sanitizer ignorelist link policy", policy: ProbeCandidatePolicyCCLink, argv: []string{"-fsanitize-ignorelist=/dev/null"}},
		{name: "randomized layout seed link policy", policy: ProbeCandidatePolicyCCLink, argv: []string{"-frandomize-layout-seed-file=/dev/null"}},
		{name: "split sanitizer ignorelist", policy: ProbeCandidatePolicyCC, argv: []string{"-fsanitize-ignorelist", "/dev/null"}},
		{name: "empty sanitizer ignorelist", policy: ProbeCandidatePolicyCC, argv: []string{"-fsanitize-ignorelist="}},
		{name: "split randomized layout seed", policy: ProbeCandidatePolicyCC, argv: []string{"-frandomize-layout-seed-file", "/dev/null"}},
		{name: "system sanitizer ignorelist", policy: ProbeCandidatePolicyCC, argv: []string{"-fsanitize-system-ignorelist=relative.scl"}},
		{name: "coverage sanitizer blacklist", policy: ProbeCandidatePolicyCC, argv: []string{"-fsanitize-coverage-blacklist=relative.scl"}},
		{name: "coverage sanitizer blocklist", policy: ProbeCandidatePolicyCC, argv: []string{"-fsanitize-coverage-blocklist=relative.scl"}},
		{name: "metadata sanitizer ignorelist", policy: ProbeCandidatePolicyCC, argv: []string{"-fexperimental-sanitize-metadata-ignorelist=relative.scl"}},
		{name: "prefix map missing mapping", policy: ProbeCandidatePolicyCC, argv: []string{"-fmacro-prefix-map"}},
		{name: "prefix map missing new separator", policy: ProbeCandidatePolicyCC, argv: []string{"-fmacro-prefix-map=old"}},
		{name: "prefix map empty old", policy: ProbeCandidatePolicyCC, argv: []string{"-fmacro-prefix-map==new"}},
		{name: "prefix map link policy", policy: ProbeCandidatePolicyCCLink, argv: []string{"-fmacro-prefix-map=old=new"}},
		{name: "plugin", policy: ProbeCandidatePolicyCC, argv: []string{"-fplugin=escape.so"}},
		{name: "pass plugin", policy: ProbeCandidatePolicyCC, argv: []string{"-fpass-plugin=escape.so"}},
		{name: "clang forwarding", policy: ProbeCandidatePolicyCC, argv: []string{"-Xclang", "-load"}},
		{name: "LLVM plugin", policy: ProbeCandidatePolicyCC, argv: []string{"-mllvm", "-load=escape.so"}},
		{name: "linker forwarded plugin", policy: ProbeCandidatePolicyCCLink, argv: []string{"-Wl,--plugin,escape.so"}},
		{name: "split B prefix", policy: ProbeCandidatePolicyCC, argv: []string{"-B", "toolchain/bin"}},
		{name: "joined B prefix", policy: ProbeCandidatePolicyCC, argv: []string{"-Btoolchain/bin"}},
		{name: "GCC toolchain", policy: ProbeCandidatePolicyCC, argv: []string{"--gcc-toolchain=toolchain"}},
		{name: "linker selection", policy: ProbeCandidatePolicyCCLink, argv: []string{"-fuse-ld=escape"}},
		{name: "resource directory", policy: ProbeCandidatePolicyCC, argv: []string{"-resource-dir", "toolchain/lib"}},
		{name: "sysroot", policy: ProbeCandidatePolicyLD, argv: []string{"--sysroot=toolchain/sysroot"}},
		{name: "unsafe linker forwarding", policy: ProbeCandidatePolicyCCLink, argv: []string{"-Xlinker", "--plugin=escape.so"}},
		{name: "unsafe assembler forwarding", policy: ProbeCandidatePolicyCC, argv: []string{"-Xassembler", "@options.rsp"}},
		{name: "empty forwarding", policy: ProbeCandidatePolicyCC, argv: []string{"-Wa,"}},
		{name: "malformed forwarding", policy: ProbeCandidatePolicyCC, argv: []string{"-Wl,-z,,now"}},
		{name: "assembler output", policy: ProbeCandidatePolicyCC, argv: []string{"-Wa,-o,escape.o"}},
		{name: "assembler listing", policy: ProbeCandidatePolicyCC, argv: []string{"-Wa,-a=escape.lst"}},
		{name: "forwarded dependency", policy: ProbeCandidatePolicyCC, argv: []string{"-Wp,-MMD,-MF,escape.d"}},
		{name: "linker script", policy: ProbeCandidatePolicyLD, argv: []string{"--script=escape.ld"}},
		{name: "default linker script", policy: ProbeCandidatePolicyLD, argv: []string{"--default-script=escape.ld"}},
		{name: "linker error script", policy: ProbeCandidatePolicyLD, argv: []string{"--error-handling-script=helper"}},
		{name: "linker remap input", policy: ProbeCandidatePolicyLD, argv: []string{"--remap-inputs-file=map"}},
		{name: "linker section ordering", policy: ProbeCandidatePolicyLD, argv: []string{"--section-ordering-file=order"}},
		{name: "linker R input", policy: ProbeCandidatePolicyLD, argv: []string{"-Rsymbols"}},
		{name: "library path", policy: ProbeCandidatePolicyCCLink, argv: []string{"-Lescape/lib"}},
		{name: "missing include", policy: ProbeCandidatePolicyCC, argv: []string{"-I"}},
		{name: "empty joined include", policy: ProbeCandidatePolicyCC, argv: []string{"-isystem="}},
		{name: "include response", policy: ProbeCandidatePolicyCC, argv: []string{"-include", "@header.rsp"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if paths, err := ValidateProbeCandidateArguments(test.policy, test.argv); err == nil {
				t.Fatalf("ValidateProbeCandidateArguments(%q, %q) = %#v, want error", test.policy, test.argv, paths)
			}
		})
	}
}

func TestShellExprArithmetic(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		command string
		want    string
	}{
		{command: `expr 6 \* 65536 + 18 \* 256 + 255`, want: "398079"},
		{command: `expr 24 / 6 / 2`, want: "2"},
		{command: `expr 2 + 3 \* 4 - 5`, want: "9"},
		{command: `expr -7 % 4`, want: "-3"},
	} {
		test := test
		t.Run(test.command, func(t *testing.T) {
			t.Parallel()
			got, err := shellExpr(test.command)
			if err != nil {
				t.Fatalf("shellExpr(%q): %v", test.command, err)
			}
			if got != test.want {
				t.Fatalf("shellExpr(%q) = %q, want %q", test.command, got, test.want)
			}
		})
	}
}

func TestShellExprRejectsInvalidArithmetic(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		`expr 1 / 0`,
		`expr 1 +`,
		`expr +1 + 2`,
		`expr 9223372036854775807 + 1`,
		`expr -9223372036854775808 \* -1`,
		`expr 1 = 1`,
	} {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			if got, err := shellExpr(command); err == nil {
				t.Fatalf("shellExpr(%q) = %q, want error", command, got)
			}
		})
	}
}
