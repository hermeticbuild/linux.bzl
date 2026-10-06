package kconfig

import (
	"reflect"
	"strings"
	"testing"
)

// TestGeneratedSourceExecutorForRecognisedKinds pins each supported generator
// against the command text the real Makefiles produce. The point of keying on
// command text is that one entry covers several spellings across kernel
// versions and architectures, so the cases below deliberately mix them.
func TestGeneratedSourceExecutorForRecognisedKinds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		object   string
		source   KbuildGeneratedSource
		want     GeneratedSourceExecutor
		wantNote string
	}{
		{
			// 6.12 arch/x86/crypto spells this cmd_perlasm; 6.12 arch/arm and
			// 6.18 lib/crypto spell it cmd_perl and cmd_perlasm respectively,
			// one with "$<" and one with "$(<)". All three canonicalise here.
			name:   "x86 perlasm",
			object: "arch/x86/crypto/poly1305-x86_64-cryptogams.o",
			source: KbuildGeneratedSource{
				Target:          "arch/x86/crypto/poly1305-x86_64-cryptogams.S",
				Primary:         "arch/x86/crypto/poly1305-x86_64-cryptogams.pl",
				Prerequisites:   []string{"arch/x86/crypto/poly1305-x86_64-cryptogams.pl"},
				CommandTemplate: "$(PERL) $< > $@",
				CommandName:     "perlasm",
			},
			want: GeneratedSourceExecutor{
				Kind:         GeneratedSourcePerlStdout,
				Target:       "arch/x86/crypto/poly1305-x86_64-cryptogams.S",
				Primary:      "arch/x86/crypto/poly1305-x86_64-cryptogams.pl",
				ActionInputs: []string{"arch/x86/crypto/poly1305-x86_64-cryptogams.pl"},
			},
		},
		{
			name:   "arm perlasm parenthesised",
			object: "arch/arm/crypto/poly1305-core.o",
			source: KbuildGeneratedSource{
				Target:          "arch/arm/crypto/poly1305-core.S",
				Primary:         "arch/arm/crypto/poly1305-armv4.pl",
				Prerequisites:   []string{"arch/arm/crypto/poly1305-armv4.pl"},
				CommandTemplate: "$(PERL) $(<) > $(@)",
				CommandName:     "perl",
			},
			want: GeneratedSourceExecutor{
				Kind:         GeneratedSourcePerlStdout,
				Target:       "arch/arm/crypto/poly1305-core.S",
				Primary:      "arch/arm/crypto/poly1305-armv4.pl",
				ActionInputs: []string{"arch/arm/crypto/poly1305-armv4.pl"},
			},
		},
		{
			// The "void" argument is read off the rule's own command text
			// rather than off an architecture table. That is the core of the
			// fix: arm64's flavour falls out of the data.
			name:   "arm64 perlasm with argument",
			object: "arch/arm64/crypto/sha256-core.o",
			source: KbuildGeneratedSource{
				Target:          "arch/arm64/crypto/sha256-core.S",
				Primary:         "arch/arm64/crypto/sha512-armv8.pl",
				Prerequisites:   []string{"arch/arm64/crypto/sha512-armv8.pl"},
				CommandTemplate: "$(PERL) $(<) void $(@)",
				CommandName:     "perlasm",
			},
			want: GeneratedSourceExecutor{
				Kind:         GeneratedSourcePerlArgOut,
				Target:       "arch/arm64/crypto/sha256-core.S",
				Primary:      "arch/arm64/crypto/sha512-armv8.pl",
				ActionInputs: []string{"arch/arm64/crypto/sha512-armv8.pl"},
				Args:         []string{"void"},
			},
		},
		{
			name:   "raid6 unroll",
			object: "lib/raid6/int4.o",
			source: KbuildGeneratedSource{
				Target:          "lib/raid6/int4.c",
				Stem:            "4",
				Primary:         "lib/raid6/int.uc",
				Prerequisites:   []string{"lib/raid6/int.uc", "lib/raid6/unroll.awk"},
				CommandTemplate: "$(AWK) -v N=$* -f lib/raid6/unroll.awk < $< > $@",
				CommandName:     "unroll",
			},
			want: GeneratedSourceExecutor{
				Kind:             GeneratedSourceRaid6Unroll,
				Target:           "lib/raid6/int4.c",
				Primary:          "lib/raid6/int.uc",
				ActionInputs:     []string{"lib/raid6/int.uc"},
				DigestOnlyInputs: []string{"lib/raid6/unroll.awk"},
				Args:             []string{"-n", "4"},
			},
		},
		{
			// $< is cpufeatures.h, but the nominal source is the script. This
			// is exactly why Primary is a per-kind function rather than
			// "prerequisite zero".
			name:   "mkcapflags",
			object: "arch/x86/kernel/cpu/capflags.o",
			source: KbuildGeneratedSource{
				Target:  "arch/x86/kernel/cpu/capflags.c",
				Primary: "arch/x86/include/asm/cpufeatures.h",
				Prerequisites: []string{
					"arch/x86/include/asm/cpufeatures.h",
					"arch/x86/include/asm/vmxfeatures.h",
					"arch/x86/kernel/cpu/mkcapflags.sh",
				},
				CommandTemplate: "$(CONFIG_SHELL) arch/x86/kernel/cpu/mkcapflags.sh $@ $^",
				CommandName:     "mkcapflags",
			},
			want: GeneratedSourceExecutor{
				Kind:    GeneratedSourceMkcapflags,
				Target:  "arch/x86/kernel/cpu/capflags.c",
				Primary: "arch/x86/kernel/cpu/mkcapflags.sh",
				ActionInputs: []string{
					"arch/x86/include/asm/cpufeatures.h",
					"arch/x86/include/asm/vmxfeatures.h",
					"arch/x86/kernel/cpu/mkcapflags.sh",
				},
			},
		},
		{
			name:   "conmakehash",
			object: "drivers/tty/vt/consolemap_deftbl.o",
			source: KbuildGeneratedSource{
				Target:          "drivers/tty/vt/consolemap_deftbl.c",
				Primary:         "drivers/tty/vt/cp437.uni",
				Prerequisites:   []string{"drivers/tty/vt/cp437.uni", "drivers/tty/vt/conmakehash"},
				CommandTemplate: "drivers/tty/vt/conmakehash $< > $@",
				CommandName:     "conmk",
			},
			want: GeneratedSourceExecutor{
				Kind:             GeneratedSourceConmakehash,
				Target:           "drivers/tty/vt/consolemap_deftbl.c",
				Primary:          "drivers/tty/vt/cp437.uni",
				ActionInputs:     []string{"drivers/tty/vt/cp437.uni"},
				DigestOnlyInputs: []string{"drivers/tty/vt/conmakehash.c"},
			},
		},
		{
			name:   "raid6 mktables",
			object: "lib/raid6/tables.o",
			source: KbuildGeneratedSource{
				Target:          "lib/raid6/tables.c",
				Primary:         "lib/raid6/mktables",
				Prerequisites:   []string{"lib/raid6/mktables"},
				CommandTemplate: "lib/raid6/mktables > $@",
				CommandName:     "mktable",
			},
			want: GeneratedSourceExecutor{
				Kind:             GeneratedSourceRaid6Mktables,
				Target:           "lib/raid6/tables.c",
				Primary:          "lib/raid6/mktables.c",
				DigestOnlyInputs: []string{"lib/raid6/mktables.c"},
				// mktables.c is a host program including <stdio.h>, so its own
				// include lines must not be scanned as the generated file's.
				ClosureInputs:   []string{"include/linux/export.h", "include/linux/raid/pq.h"},
				SkipPrimaryScan: true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GeneratedSourceExecutorFor(tc.object, tc.source)
			if err != nil {
				t.Fatalf("GeneratedSourceExecutorFor() failed: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("executor mismatch\nwant: %#v\n got: %#v", tc.want, got)
			}
		})
	}
}

// TestGeneratedSourceExecutorForResolvedInterpreterPath pins that a consumer
// who defines PERL through "-var" or module_make_vars gets the same
// classification as the usual literal "$(PERL)". PERL is absent from the
// default variable environment only because just arch/<srcarch>/Makefile is
// parsed, which is a fact about this parser rather than an invariant.
func TestGeneratedSourceExecutorForResolvedInterpreterPath(t *testing.T) {
	got, err := GeneratedSourceExecutorFor("arch/x86/crypto/poly1305-x86_64-cryptogams.o", KbuildGeneratedSource{
		Target:          "arch/x86/crypto/poly1305-x86_64-cryptogams.S",
		Primary:         "arch/x86/crypto/poly1305-x86_64-cryptogams.pl",
		Prerequisites:   []string{"arch/x86/crypto/poly1305-x86_64-cryptogams.pl"},
		CommandTemplate: "/usr/bin/perl $< > $@",
		CommandName:     "perlasm",
	})
	if err != nil {
		t.Fatalf("GeneratedSourceExecutorFor() failed: %v", err)
	}
	if got.Kind != GeneratedSourcePerlStdout {
		t.Fatalf("kind = %q, want %q", got.Kind, GeneratedSourcePerlStdout)
	}
}

// TestGeneratedSourceExecutorForRejectsUnknownGenerators pins the failure
// contract. These messages are what a contributor sees when a new kernel
// introduces a generator, so they are asserted rather than merely non-empty.
func TestGeneratedSourceExecutorForRejectsUnknownGenerators(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source KbuildGeneratedSource
		want   []string
	}{
		{
			// powerpc and mips perlasm. Unreachable on the supported
			// architectures and deliberately unimplemented: those scripts
			// spawn an "*-xlate.pl" child the sandbox cannot declare.
			name: "perlasm with flavour argument",
			source: KbuildGeneratedSource{
				Target:          "arch/powerpc/crypto/aesp10-ppc.S",
				Primary:         "arch/powerpc/crypto/aesp10-ppc.pl",
				Prerequisites:   []string{"arch/powerpc/crypto/aesp10-ppc.pl"},
				CommandTemplate: "$(PERL) $< linux-ppc64le $@",
				CommandName:     "perl",
				Position:        Position{Filename: "arch/powerpc/crypto/Makefile", Line: 53},
			},
			want: []string{
				`no generator is implemented for leaf object "arch/powerpc/crypto/aesp10-ppc.o"`,
				`whose source "arch/powerpc/crypto/aesp10-ppc.S" is produced by cmd_perl at arch/powerpc/crypto/Makefile:53`,
				"canonical: PERL $< linux-ppc64le $@",
				"internal/kconfig/generated_source_kinds.go",
				"_linux_generated_source in internal/linux_objects.bzl",
			},
		},
		{
			// A surviving non-automatic reference means the command was not
			// fully understood, so it must not be matched against the table.
			name: "unresolved make reference",
			source: KbuildGeneratedSource{
				Target:          "lib/crypto/poly1305-core.S",
				Primary:         "lib/crypto/poly1305-riscv.pl",
				Prerequisites:   []string{"lib/crypto/poly1305-riscv.pl"},
				CommandTemplate: "$(PERL) $< $(poly1305-perlasm-flavour-y) $@",
				CommandName:     "perlasm_poly1305",
				Position:        Position{Filename: "lib/crypto/Makefile", Line: 166},
			},
			want: []string{
				"still references $(poly1305-perlasm-flavour-y), which is not an automatic variable",
			},
		},
		{
			name: "unrecognised program",
			source: KbuildGeneratedSource{
				Target:          "drivers/thing/widget.c",
				Primary:         "drivers/thing/widget.in",
				Prerequisites:   []string{"drivers/thing/widget.in"},
				CommandTemplate: "$(PYTHON3) drivers/thing/gen.py $< > $@",
				CommandName:     "genwidget",
				Position:        Position{Filename: "drivers/thing/Makefile", Line: 7},
			},
			want: []string{
				`the leading program word "$(PYTHON3)" is an unrecognised make reference`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := strings.TrimSuffix(tc.source.Target, ".S")
			object = strings.TrimSuffix(object, ".c") + ".o"
			_, err := GeneratedSourceExecutorFor(object, tc.source)
			if err == nil {
				t.Fatalf("GeneratedSourceExecutorFor() succeeded, want a failure")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error is missing %q\nerror: %v", want, err)
				}
			}
		})
	}
}
