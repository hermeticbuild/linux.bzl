package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func compilerIntrinsicCandidateStepForTest(operators, argv []string) ProbeStep {
	step := intrinsicSourceShapeTestRequest().Steps[0]
	step.Stdin = ""
	for _, operator := range operators {
		step.Stdin += "#undef terminal_operand\n" + operator + "(terminal_operand)\n"
	}
	step.Arguments = append(slices.Clone(argv), "-E", "-P", "-x", "c", "-")
	step.Candidate.Base = make([]int, len(argv))
	for index := range argv {
		step.Candidate.Base[index] = index
	}
	return step
}

func TestCompilerIntrinsicCandidateProtectsOnlyInvokedOperators(t *testing.T) {
	for _, operators := range [][]string{{"__has_attribute"}, {"__has_builtin"}, {"__has_attribute", "__has_builtin"}} {
		for _, modified := range []string{"__has_attribute", "__has_builtin"} {
			for _, override := range [][]string{
				{"-D" + modified + "(x)=0"}, {"-D", modified + "(x)=0"},
				{"-D" + modified + "=0"}, {"-D", modified + "=0"},
				{"-U" + modified}, {"-U", modified},
			} {
				t.Run(strings.Join(operators, "+")+"/"+strings.Join(override, " "), func(t *testing.T) {
					argv := append([]string{"-nostdinc"}, override...)
					original := slices.Clone(argv)
					step := compilerIntrinsicCandidateStepForTest(operators, argv)
					paths, err := ValidateCompilerIntrinsicProbeCandidateArguments(step, argv)
					wantAllowed := !slices.Contains(operators, modified)
					if (err == nil) != wantAllowed || len(paths) != 0 {
						t.Fatalf("source-specific operator protection allowed=%t, want %t: %#v %v", err == nil, wantAllowed, paths, err)
					}
					if !slices.Equal(argv, original) || !slices.Equal(step.Arguments[:len(argv)], original) {
						t.Fatal("source-aware operator protection changed original compiler words")
					}
					// The source-less API retains its exact old attribute-only
					// protection; it must not silently acquire a new builtin ban.
					_, legacyErr := ValidateProjectedProbeCandidateArguments(ProbeCandidatePolicyCC, ProbeCandidateProjectionCompilerIntrinsic, argv)
					if (legacyErr == nil) != (modified == "__has_builtin") {
						t.Fatalf("source-less projected validation changed legacy behavior: %v", legacyErr)
					}
				})
			}
		}
	}
}

func TestCompilerIntrinsicCandidateMixedOperatorProtectionHasNoMacroBypass(t *testing.T) {
	operators := []string{"__has_attribute", "__has_builtin"}
	for _, operator := range operators {
		for _, tail := range [][]string{
			{"-D" + operator + "(x)=_Pragma(HEADER)"},
			{"-DWRAP(x)=_Pragma(HEADER)", "-D" + operator + "=WRAP"},
			{"-D" + operator + "(x)=1", "-U" + operator},
			{"--define-macro=" + operator + "(x)=_Pragma(HEADER)"},
			{"--define", operator + "(x)=_Pragma(HEADER)"},
			{"--d=" + operator + "(x)=_Pragma(HEADER)"},
			{"--undefine-macro=" + operator}, {"--u", operator},
			{"-Wp,-D" + operator + "(x)=_Pragma(HEADER)"},
			{"-Wa,-D" + operator + "(x)=_Pragma(HEADER)"},
			{"-Wl,-D" + operator + "(x)=_Pragma(HEADER)"},
		} {
			t.Run(strings.Join(tail, " "), func(t *testing.T) {
				argv := append([]string{`-DHEADER="GCC dependency /not/declared"`}, tail...)
				step := compilerIntrinsicCandidateStepForTest(operators, argv)
				if paths, err := ValidateCompilerIntrinsicProbeCandidateArguments(step, argv); err == nil || len(paths) != 0 {
					t.Fatalf("mixed source admitted an indirect operator override: %#v %v", paths, err)
				}
			})
		}
	}
}

func TestCompilerIntrinsicCandidateSourceDoesNotGrantFilesystemAuthority(t *testing.T) {
	for _, operators := range [][]string{{"__has_attribute"}, {"__has_builtin"}, {"__has_attribute", "__has_builtin"}} {
		argv := []string{`-DUNRELATED="/not/an/input"`, "-I", "/declared/include"}
		step := compilerIntrinsicCandidateStepForTest(operators, argv)
		paths, err := ValidateCompilerIntrinsicProbeCandidateArguments(step, argv)
		want := []ProbeCandidatePathOperand{{Kind: ProbeCandidatePathInclude, Argument: 2, End: len("/declared/include")}}
		if err != nil || !slices.Equal(paths, want) {
			t.Fatalf("literal exception erased adjacent include-path ownership: %#v %v", paths, err)
		}
		for _, tail := range []string{"/not/declared.c", "--future-path=/not/declared", "-fplugin=/not/declared.so", `-DOTHER=__has_include("/not/declared")`} {
			argv := []string{`-DUNRELATED="/not/an/input"`, tail}
			step := compilerIntrinsicCandidateStepForTest(operators, argv)
			if paths, err := ValidateCompilerIntrinsicProbeCandidateArguments(step, argv); err == nil || len(paths) != 0 {
				t.Fatalf("canonical intrinsic source granted unrelated file access: %#v %v", paths, err)
			}
		}
	}
}

func TestProbeCandidateLiteralMacroDefinition(t *testing.T) {
	for _, test := range []struct {
		name, operand string
		want          bool
	}{
		{"reported Kbuild definition", `KBUILD_MODFILE="__LINUX_BZL_OBJECT_TREE__/arch/x86/boot/regs"`, true},
		{"arbitrary object name", `OTHER_NAME_9="__LINUX_BZL_SOURCE_TREE__/drivers/example.c"`, true},
		{"absolute path", `ROOT="/not/a/declared/input"`, true},
		{"empty string", `EMPTY=""`, true},
		{"underscore name", `_="../relative/path"`, true},
		{"literal dollar and response marker", `DATA="$ROOT/@response/file"`, true},
		{"escaped backslashes", `WIN="C:\\not\\an\\input"`, true},
		{"escaped quotes", `QUOTED="/path/\"quoted\""`, true},
		{"portable simple escapes", `ESC="/\a\b\f\n\r\t\v\'\?\\\""`, true},
		{"one to three octal digits", `OCTAL="/\0\12\1234"`, true},
		{"hexadecimal escapes", `HEX="/\x2f\x0041"`, true},
		{"no replacement", `NAME`, false},
		{"no equals", `NAME"/path"`, false},
		{"empty replacement", `NAME=`, false},
		{"empty name", `="/path"`, false},
		{"numeric name", `9NAME="/path"`, false},
		{"non ASCII name", `NÁME="/path"`, false},
		{"name slash", `NA/ME="/path"`, false},
		{"name space", `NAME OTHER="/path"`, false},
		{"name comment", `NAME/**/="/path"`, false},
		{"function macro", `NAME(x)="/path"`, false},
		{"unclosed quote", `NAME="/path`, false},
		{"escaped closing quote", `NAME="/path\"`, false},
		{"trailing backslash", `NAME="/path\`, false},
		{"trailing identifier", `NAME="/path"OTHER`, false},
		{"trailing comment", `NAME="/path"/**/`, false},
		{"concatenated strings", `NAME="/path""/other"`, false},
		{"parenthesized string", `NAME=("/path")`, false},
		{"wide string", `NAME=L"/path"`, false},
		{"UTF8 string prefix", `NAME=u8"/path"`, false},
		{"character literal", `NAME='/'`, false},
		{"unknown escape", `NAME="/path\q"`, false},
		{"invalid octal escape", `NAME="/path\8"`, false},
		{"hexadecimal escape without digits", `NAME="/path\x"`, false},
		{"universal escape", `NAME="/path\u002f"`, false},
		{"trigraph backslash", `NAME="/path??/"`, false},
		{"trigraph hash", `NAME="/path??="`, false},
		{"pragma replacement", `NAME=_Pragma("GCC dependency /not/declared")`, false},
		{"include predicate replacement", `NAME=__has_include("/not/declared")`, false},
		{"line feed", "NAME=\"/path\nother\"", false},
		{"carriage return", "NAME=\"/path\rother\"", false},
		{"NUL", "NAME=\"/path\x00other\"", false},
		{"raw tab", "NAME=\"/path\tother\"", false},
		{"DEL", "NAME=\"/path\x7fother\"", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := probeCandidateLiteralMacroDefinition(test.operand); got != test.want {
				t.Fatalf("literal macro admission = %t, want %t for %q", got, test.want, test.operand)
			}
		})
	}
}

func TestValidateProjectedProbeCandidateArgumentsPreservesLiteralMacrosAndPathSpans(t *testing.T) {
	for _, definition := range []string{
		`KBUILD_MODFILE="__LINUX_BZL_OBJECT_TREE__/arch/x86/boot/regs"`,
		`OTHER="__LINUX_BZL_SOURCE_TREE__/drivers/file.c"`,
		`ABSOLUTE="/not/an/input"`,
		`ESCAPED="C:\\directory\\\"quoted\"/$name/@options"`,
	} {
		for _, split := range []bool{false, true} {
			t.Run(definition+map[bool]string{false: "/attached", true: "/split"}[split], func(t *testing.T) {
				arguments := []string{"-D" + definition}
				if split {
					arguments = []string{"-D", definition}
				}
				original := slices.Clone(arguments)
				paths, err := ValidateProjectedProbeCandidateArguments(ProbeCandidatePolicyCC, ProbeCandidateProjectionCompilerIntrinsic, arguments)
				if err != nil || len(paths) != 0 {
					t.Fatalf("literal macro path classification = %#v, %v, want no paths", paths, err)
				}
				if !slices.Equal(arguments, original) {
					t.Fatal("literal macro validation changed original compiler words")
				}

				includeIndex := len(arguments)
				arguments = append(arguments, "-I", "/declared/include", "-include", "/declared/forced.h", "-Ijoined/include")
				original = slices.Clone(arguments)
				paths, err = ValidateProjectedProbeCandidateArguments(ProbeCandidatePolicyCC, ProbeCandidateProjectionCompilerIntrinsic, arguments)
				if err != nil {
					t.Fatal(err)
				}
				want := []ProbeCandidatePathOperand{
					{Kind: ProbeCandidatePathInclude, Argument: includeIndex + 1, Start: 0, End: len("/declared/include")},
					{Kind: ProbeCandidatePathForcedInclude, Argument: includeIndex + 3, Start: 0, End: len("/declared/forced.h")},
					{Kind: ProbeCandidatePathInclude, Argument: includeIndex + 4, Start: len("-I"), End: len("-Ijoined/include")},
				}
				if !slices.Equal(paths, want) {
					t.Fatalf("adjacent filesystem operands = %#v, want %#v", paths, want)
				}
				if !slices.Equal(arguments, original) {
					t.Fatal("path classification changed original macro or filesystem arguments")
				}
			})
		}
	}
}

func TestProjectProbeCandidateArgumentsIntrinsicKeepsLiteralMacroOrigins(t *testing.T) {
	arguments := []string{
		"-I", "source/include", "-nostdinc",
		`-DKBUILD_MODFILE="__LINUX_BZL_OBJECT_TREE__/arch/x86/boot/regs"`,
		"-D", `OTHER="/path/\"quoted\""`, "-UOTHER", "-D", `OTHER="/last/value"`,
		"-c", "source.c", "-o", "source.o", "-MMD", "-MF", "source.d",
	}
	original := slices.Clone(arguments)
	projected, origins, err := ProjectProbeCandidateArguments(ProbeCandidateProjectionCompilerIntrinsic, arguments, []string{"source.c"})
	if err != nil {
		t.Fatal(err)
	}
	wantOrigins := []int{2, 3, 4, 5, 6, 7, 8}
	want := []string{original[2], original[3], original[4], original[5], original[6], original[7], original[8]}
	if !slices.Equal(projected, want) || !slices.Equal(origins, wantOrigins) {
		t.Fatalf("intrinsic literal projection = %q at %v, want %q at %v", projected, origins, want, wantOrigins)
	}
	paths, err := ValidateProjectedProbeCandidateArguments(ProbeCandidatePolicyCC, ProbeCandidateProjectionCompilerIntrinsic, projected)
	if err != nil || len(paths) != 0 {
		t.Fatalf("projected literal macro candidate = %#v, %v", paths, err)
	}
	if !slices.Equal(arguments, original) || !slices.Equal(projected, want) {
		t.Fatal("projection or validation mutated exact literal replacement bytes or ordering")
	}
}

func TestValidateProjectedProbeCandidateArgumentsRejectsUnprovedMacroAuthority(t *testing.T) {
	for _, test := range []struct {
		name string
		argv []string
	}{
		{"split response file", []string{"-D", "@options.rsp"}},
		{"response file", []string{"@options.rsp"}},
		{"missing split definition", []string{"-D"}},
		{"missing undefinition", []string{"-U"}},
		{"missing definition before managed boundary", []string{"-D", "-E"}},
		{"empty macro name", []string{`-D="/path"`}},
		{"numeric macro name", []string{`-D9BAD="/path"`}},
		{"non ASCII macro name", []string{`-DNÁME="/path"`}},
		{"function signature trailing tokens", []string{`-DF(x)junk=1`}},
		{"undefinition replacement", []string{"-UOTHER=1"}},
		{"control byte", []string{"-DNAME=\"/path\nother\""}},
		{"unclosed literal", []string{`-DNAME="/path`}},
		{"unknown escape", []string{`-DNAME="/path\q"`}},
		{"trigraph", []string{`-DNAME="/path??/"`}},
		{"literal then token", []string{`-DNAME="/path"OTHER`}},
		{"literal then comment", []string{`-DNAME="/path"/**/`}},
		{"concatenated literal", []string{`-DNAME="/path""/other"`}},
		{"pragma path body", []string{`-DNAME=_Pragma("GCC dependency /not/declared")`}},
		{"include predicate path body", []string{`-DNAME=__has_include("/not/declared")`}},
		{"function path body", []string{`-DF(x)="/not/declared"`}},
		{"function predicate body", []string{`-DF(x)=__has_include("/not/declared")`}},
		{"attached operator definition", []string{"-D__has_attribute=1"}},
		{"split operator definition", []string{"-D", "__has_attribute=1"}},
		{"operator no replacement", []string{"-D__has_attribute"}},
		{"operator function definition", []string{"-D__has_attribute(x)=1"}},
		{"split operator function definition", []string{"-D", "__has_attribute(x)=1"}},
		{"operator literal definition", []string{`-D__has_attribute="/path"`}},
		{"attached operator undefinition", []string{"-U__has_attribute"}},
		{"split operator undefinition", []string{"-U", "__has_attribute"}},
		{"later operator restoration", []string{"-D__has_attribute=1", "-U__has_attribute"}},
		{"pragma composition", []string{`-DHEADER="GCC dependency /not/declared"`, "-D__has_attribute(x)=_Pragma(HEADER)"}},
		{"indirect pragma composition", []string{`-DHEADER="GCC dependency /not/declared"`, "-DWRAP(x)=_Pragma(HEADER)", "-D__has_attribute=WRAP"}},
		{"safe literal does not authorize following path", []string{`-DNAME="/literal"`, "--future-path=/not/declared"}},
		{"safe literal does not authorize plugin", []string{`-DNAME="/literal"`, "-fplugin=/not/declared.so"}},
		{"safe literal does not authorize positional input", []string{`-DNAME="/literal"`, "/not/declared.c"}},
		{"assembler forwarding", []string{`-Wa,-DNAME="/not/declared"`}},
		{"preprocessor forwarding", []string{`-Wp,-DNAME="/not/declared"`}},
		{"linker forwarding", []string{`-Wl,-DNAME="/not/declared"`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := slices.Clone(test.argv)
			if paths, err := ValidateProjectedProbeCandidateArguments(ProbeCandidatePolicyCC, ProbeCandidateProjectionCompilerIntrinsic, test.argv); err == nil {
				t.Fatalf("unproved macro or filesystem authority accepted: %q, paths %#v", test.argv, paths)
			}
			if !slices.Equal(test.argv, original) {
				t.Fatal("rejected candidate mutated caller arguments")
			}
		})
	}
}

func TestValidateProjectedProbeCandidateArgumentsLiteralExceptionIsIntrinsicOnly(t *testing.T) {
	for _, policy := range []string{ProbeCandidatePolicyCC, ProbeCandidatePolicyCCLink, ProbeCandidatePolicyLD} {
		for _, arguments := range [][]string{
			{`-DNAME="/not/declared"`},
			{"-D", `NAME="/not/declared"`},
		} {
			if _, err := ValidateProbeCandidateArguments(policy, arguments); err == nil {
				t.Fatalf("generic %s validator acquired literal macro exception for %q", policy, arguments)
			}
			for _, projection := range []string{"", ProbeCandidateProjectionCompilerPredefines, ProbeCandidateProjectionCompilerIntrinsic} {
				if policy == ProbeCandidatePolicyCC && projection == ProbeCandidateProjectionCompilerIntrinsic {
					continue
				}
				if _, err := ValidateProjectedProbeCandidateArguments(policy, projection, arguments); err == nil {
					t.Fatalf("%s/%s acquired intrinsic-only literal macro exception for %q", policy, projection, arguments)
				}
			}
		}
	}
	for _, forwarding := range []string{"-Wa,", "-Wp,", "-Wl,"} {
		if _, err := ValidateProbeCandidateArguments(ProbeCandidatePolicyCC, []string{forwarding + `-DNAME="/not/declared"`}); err == nil {
			t.Fatalf("generic forwarding %s acquired literal macro exception", forwarding)
		}
	}
}

func TestValidateProjectedProbeCandidateArgumentsKeepsExistingNonpathMacroInputs(t *testing.T) {
	arguments := []string{"-DPLAIN", "-DVALUE=202311", "-D", "F(x)=((x)+1)", "-U", "OLD", "-UOTHER", "-D__has_attribute_suffix=1"}
	original := slices.Clone(arguments)
	for _, projection := range []string{"", ProbeCandidateProjectionCompilerPredefines, ProbeCandidateProjectionCompilerIntrinsic} {
		paths, err := ValidateProjectedProbeCandidateArguments(ProbeCandidatePolicyCC, projection, arguments)
		if err != nil || len(paths) != 0 {
			t.Fatalf("existing nonpath macro candidate under %q = %#v, %v", projection, paths, err)
		}
		if !slices.Equal(arguments, original) {
			t.Fatal("nonpath macro validation mutated compiler arguments")
		}
	}
}

func TestCompilerIntrinsicProjectionRejectsMacroOptionAliases(t *testing.T) {
	// An alias can redefine the intrinsic without a -D-looking argument. With
	// the quoted-string exception enabled, that could expand an otherwise inert
	// path literal through _Pragma. Long-option abbreviation is deliberately
	// rejected conservatively, without selecting a compiler family at runtime.
	for _, alias := range []struct {
		option, operand string
	}{
		{"--define-macro", "__has_attribute(x)=_Pragma(HEADER)"},
		{"--define-macr", "__has_attribute(x)=_Pragma(HEADER)"},
		{"--define", "__has_attribute(x)=_Pragma(HEADER)"},
		{"--d", "__has_attribute(x)=_Pragma(HEADER)"},
		{"--undefine-macro", "__has_attribute"},
		{"--undefine-macr", "__has_attribute"},
		{"--undefine", "__has_attribute"},
		{"--u", "__has_attribute"},
	} {
		for _, split := range []bool{false, true} {
			t.Run(alias.option+map[bool]string{false: "/equals", true: "/split"}[split], func(t *testing.T) {
				arguments := []string{`-DHEADER="GCC dependency /not/declared"`}
				if split {
					arguments = append(arguments, alias.option, alias.operand)
				} else {
					arguments = append(arguments, alias.option+"="+alias.operand)
				}
				original := slices.Clone(arguments)
				if _, err := ValidateProjectedProbeCandidateArguments(ProbeCandidatePolicyCC, ProbeCandidateProjectionCompilerIntrinsic, arguments); err == nil {
					t.Fatalf("intrinsic candidate accepted macro alias %q", arguments)
				}
				if _, _, err := ProjectProbeCandidateArguments(ProbeCandidateProjectionCompilerIntrinsic, arguments, nil); err == nil {
					t.Fatalf("intrinsic projection accepted macro alias %q", arguments)
				}
				if !slices.Equal(arguments, original) {
					t.Fatal("rejected macro alias mutated original compiler arguments")
				}
			})
		}
	}
}
