package kconfig

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func generatedSourceForTest(t *testing.T, files map[string]string, object string) (KbuildGeneratedSource, bool, error) {
	t.Helper()
	root := t.TempDir()
	for name, contents := range files {
		mustWriteSource(t, root, name, contents)
	}
	kb, err := ParseKbuildDirectoryTree(filepath.Join(root, "Makefile"), KbuildOptions{RootDir: root})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	return kb.GeneratedSourceResolver().ForObject(object)
}

// rootMakefileDescending builds a top-level Makefile that descends into dir,
// which is how the real traversal reaches a per-directory Makefile.
func rootMakefileDescending(dirs ...string) string {
	var out strings.Builder
	for _, dir := range dirs {
		out.WriteString("obj-y += " + dir + "/\n")
	}
	return out.String()
}

func TestGeneratedSourceForObjectX86CryptoPerlasm(t *testing.T) {
	// arch/x86/crypto/Makefile in 6.12.96, verbatim shape.
	got, ok, err := generatedSourceForTest(t, map[string]string{
		"Makefile": rootMakefileDescending("arch/x86/crypto"),
		"arch/x86/crypto/Makefile": `obj-$(CONFIG_CRYPTO_POLY1305_X86_64) += poly1305-x86_64.o
poly1305-x86_64-y := poly1305-x86_64-cryptogams.o poly1305_glue.o
targets += poly1305-x86_64-cryptogams.S

quiet_cmd_perlasm = PERLASM $@
      cmd_perlasm = $(PERL) $< > $@
$(obj)/%.S: $(src)/%.pl FORCE
	$(call if_changed,perlasm)
`,
		"arch/x86/crypto/poly1305-x86_64-cryptogams.pl": "# perlasm\n",
	}, "arch/x86/crypto/poly1305-x86_64-cryptogams.o")
	if err != nil {
		t.Fatalf("ForObject() failed: %v", err)
	}
	if !ok {
		t.Fatalf("ForObject() found no generated source")
	}
	assertGeneratedSource(t, got, KbuildGeneratedSource{
		Target:          "arch/x86/crypto/poly1305-x86_64-cryptogams.S",
		Stem:            "poly1305-x86_64-cryptogams",
		Primary:         "arch/x86/crypto/poly1305-x86_64-cryptogams.pl",
		Prerequisites:   []string{"arch/x86/crypto/poly1305-x86_64-cryptogams.pl"},
		CommandTemplate: "$(PERL) $< > $@",
		CommandName:     "perlasm",
		Directory:       "arch/x86/crypto",
	})
}

func TestGeneratedSourceForObjectRaid6Unroll(t *testing.T) {
	// lib/raid6/Makefile in 6.12.96. All five ".uc" families share one
	// cmd_unroll; only int%.c is reachable on the supported architectures.
	files := map[string]string{
		"Makefile": rootMakefileDescending("lib/raid6"),
		"lib/raid6/Makefile": `obj-$(CONFIG_RAID6_PQ) += raid6_pq.o
raid6_pq-y += algos.o recov.o tables.o int1.o int2.o int4.o int8.o
hostprogs += mktables

quiet_cmd_unroll = UNROLL  $@
      cmd_unroll = $(AWK) -v N=$* -f $(src)/unroll.awk < $< > $@

targets += int1.c int2.c int4.c int8.c
$(obj)/int%.c: $(src)/int.uc $(src)/unroll.awk FORCE
	$(call if_changed,unroll)

quiet_cmd_mktable = TABLE   $@
      cmd_mktable = $(obj)/mktables > $@
targets += tables.c
$(obj)/tables.c: $(obj)/mktables FORCE
	$(call cmd,mktable)
`,
		"lib/raid6/int.uc":     "/* unrolled */\n",
		"lib/raid6/unroll.awk": "# unroll\n",
		"lib/raid6/mktables.c": "int main(void) { return 0; }\n",
	}

	for _, tc := range []struct {
		object string
		want   KbuildGeneratedSource
	}{
		{
			object: "lib/raid6/int1.o",
			want: KbuildGeneratedSource{
				Target:          "lib/raid6/int1.c",
				Stem:            "1",
				Primary:         "lib/raid6/int.uc",
				Prerequisites:   []string{"lib/raid6/int.uc", "lib/raid6/unroll.awk"},
				CommandTemplate: "$(AWK) -v N=$* -f lib/raid6/unroll.awk < $< > $@",
				CommandName:     "unroll",
				Directory:       "lib/raid6",
			},
		},
		{
			object: "lib/raid6/int8.o",
			want: KbuildGeneratedSource{
				Target:          "lib/raid6/int8.c",
				Stem:            "8",
				Primary:         "lib/raid6/int.uc",
				Prerequisites:   []string{"lib/raid6/int.uc", "lib/raid6/unroll.awk"},
				CommandTemplate: "$(AWK) -v N=$* -f lib/raid6/unroll.awk < $< > $@",
				CommandName:     "unroll",
				Directory:       "lib/raid6",
			},
		},
		{
			// tables.c is an explicit rule whose only prerequisite is a
			// hostprog that does not exist as a checked-in file. Explicit
			// rules deliberately skip the prerequisite-existence test.
			object: "lib/raid6/tables.o",
			want: KbuildGeneratedSource{
				Target:          "lib/raid6/tables.c",
				Primary:         "lib/raid6/mktables",
				Prerequisites:   []string{"lib/raid6/mktables"},
				CommandTemplate: "lib/raid6/mktables > $@",
				CommandName:     "mktable",
				Directory:       "lib/raid6",
			},
		},
	} {
		t.Run(tc.object, func(t *testing.T) {
			got, ok, err := generatedSourceForTest(t, files, tc.object)
			if err != nil {
				t.Fatalf("ForObject(%q) failed: %v", tc.object, err)
			}
			if !ok {
				t.Fatalf("ForObject(%q) found no generated source", tc.object)
			}
			assertGeneratedSource(t, got, tc.want)
		})
	}
}

// TestGeneratedSourceForObjectArm64ExplicitOverride is the case the hardcoded
// object table got wrong: 6.12's arch/arm64/crypto/Makefile carries a pattern
// rule and then an explicit rule for one of the targets it would match. GNU
// make prefers the explicit rule, so sha256-core.S comes from sha512-armv8.pl.
func TestGeneratedSourceForObjectArm64ExplicitOverride(t *testing.T) {
	files := map[string]string{
		"Makefile": rootMakefileDescending("arch/arm64/crypto"),
		"arch/arm64/crypto/Makefile": `obj-$(CONFIG_CRYPTO_SHA256_ARM64) += sha256-arm64.o
sha256-arm64-y := sha256-core.o sha256-glue.o
obj-$(CONFIG_CRYPTO_SHA512_ARM64) += sha512-arm64.o
sha512-arm64-y := sha512-core.o sha512-glue.o

quiet_cmd_perlasm = PERLASM $@
      cmd_perlasm = $(PERL) $(<) void $(@)

$(obj)/%-core.S: $(src)/%-armv8.pl
	$(call cmd,perlasm)

$(obj)/sha256-core.S: $(src)/sha512-armv8.pl
	$(call cmd,perlasm)
`,
		"arch/arm64/crypto/sha512-armv8.pl":   "# perlasm\n",
		"arch/arm64/crypto/poly1305-armv8.pl": "# perlasm\n",
	}

	for _, tc := range []struct {
		object string
		want   KbuildGeneratedSource
	}{
		{
			object: "arch/arm64/crypto/sha256-core.o",
			want: KbuildGeneratedSource{
				Target:          "arch/arm64/crypto/sha256-core.S",
				Primary:         "arch/arm64/crypto/sha512-armv8.pl",
				Prerequisites:   []string{"arch/arm64/crypto/sha512-armv8.pl"},
				CommandTemplate: "$(PERL) $(<) void $(@)",
				CommandName:     "perlasm",
				Directory:       "arch/arm64/crypto",
			},
		},
		{
			object: "arch/arm64/crypto/sha512-core.o",
			want: KbuildGeneratedSource{
				Target:          "arch/arm64/crypto/sha512-core.S",
				Stem:            "sha512",
				Primary:         "arch/arm64/crypto/sha512-armv8.pl",
				Prerequisites:   []string{"arch/arm64/crypto/sha512-armv8.pl"},
				CommandTemplate: "$(PERL) $(<) void $(@)",
				CommandName:     "perlasm",
				Directory:       "arch/arm64/crypto",
			},
		},
		{
			object: "arch/arm64/crypto/poly1305-core.o",
			want: KbuildGeneratedSource{
				Target:          "arch/arm64/crypto/poly1305-core.S",
				Stem:            "poly1305",
				Primary:         "arch/arm64/crypto/poly1305-armv8.pl",
				Prerequisites:   []string{"arch/arm64/crypto/poly1305-armv8.pl"},
				CommandTemplate: "$(PERL) $(<) void $(@)",
				CommandName:     "perlasm",
				Directory:       "arch/arm64/crypto",
			},
		},
	} {
		t.Run(tc.object, func(t *testing.T) {
			got, ok, err := generatedSourceForTest(t, files, tc.object)
			if err != nil {
				t.Fatalf("ForObject(%q) failed: %v", tc.object, err)
			}
			if !ok {
				t.Fatalf("ForObject(%q) found no generated source", tc.object)
			}
			assertGeneratedSource(t, got, tc.want)
		})
	}
}

// TestGeneratedSourceForObjectCapflags covers the "$(cpufeature)" indirection,
// which expands to a "$(src)/../../include/..." path and therefore only
// normalises correctly if the resolver cleans it.
func TestGeneratedSourceForObjectCapflags(t *testing.T) {
	got, ok, err := generatedSourceForTest(t, map[string]string{
		"Makefile": rootMakefileDescending("arch/x86/kernel/cpu"),
		"arch/x86/kernel/cpu/Makefile": `obj-y += capflags.o

quiet_cmd_mkcapflags = MKCAP   $@
      cmd_mkcapflags = $(CONFIG_SHELL) $(src)/mkcapflags.sh $@ $^

cpufeature = $(src)/../../include/asm/cpufeatures.h
vmxfeature = $(src)/../../include/asm/vmxfeatures.h

$(obj)/capflags.c: $(cpufeature) $(vmxfeature) $(src)/mkcapflags.sh FORCE
	$(call if_changed,mkcapflags)
targets += capflags.c
`,
		"arch/x86/include/asm/cpufeatures.h": "/* features */\n",
		"arch/x86/include/asm/vmxfeatures.h": "/* vmx */\n",
		"arch/x86/kernel/cpu/mkcapflags.sh":  "#!/bin/sh\n",
	}, "arch/x86/kernel/cpu/capflags.o")
	if err != nil {
		t.Fatalf("ForObject() failed: %v", err)
	}
	if !ok {
		t.Fatalf("ForObject() found no generated source")
	}
	assertGeneratedSource(t, got, KbuildGeneratedSource{
		Target:  "arch/x86/kernel/cpu/capflags.c",
		Primary: "arch/x86/include/asm/cpufeatures.h",
		Prerequisites: []string{
			"arch/x86/include/asm/cpufeatures.h",
			"arch/x86/include/asm/vmxfeatures.h",
			"arch/x86/kernel/cpu/mkcapflags.sh",
		},
		CommandTemplate: "$(CONFIG_SHELL) arch/x86/kernel/cpu/mkcapflags.sh $@ $^",
		CommandName:     "mkcapflags",
		Directory:       "arch/x86/kernel/cpu",
	})
}

// TestGeneratedSourceForObjectConsolemap covers "$(FONTMAPFILE)" resolving to
// cp437.uni, and pins that the recipe-less "defkeymap.o: defkeymap.c" rule in
// the same Makefile is ignored rather than hijacking defkeymap.o away from its
// checked-in "_shipped" source.
func TestGeneratedSourceForObjectConsolemap(t *testing.T) {
	files := map[string]string{
		"Makefile": rootMakefileDescending("drivers/tty/vt"),
		"drivers/tty/vt/Makefile": `FONTMAPFILE = cp437.uni

obj-$(CONFIG_VT) += vt.o defkeymap.o
obj-$(CONFIG_CONSOLE_TRANSLATIONS) += consolemap.o consolemap_deftbl.o

hostprogs += conmakehash

quiet_cmd_conmk = CONMK   $@
      cmd_conmk = $(obj)/conmakehash $< > $@

$(obj)/consolemap_deftbl.c: $(src)/$(FONTMAPFILE) $(obj)/conmakehash
	$(call cmd,conmk)

$(obj)/defkeymap.o:  $(obj)/defkeymap.c
`,
		"drivers/tty/vt/cp437.uni":           "# font map\n",
		"drivers/tty/vt/conmakehash.c":       "int main(void) { return 0; }\n",
		"drivers/tty/vt/defkeymap.c_shipped": "/* shipped */\n",
	}

	got, ok, err := generatedSourceForTest(t, files, "drivers/tty/vt/consolemap_deftbl.o")
	if err != nil {
		t.Fatalf("ForObject() failed: %v", err)
	}
	if !ok {
		t.Fatalf("ForObject() found no generated source")
	}
	assertGeneratedSource(t, got, KbuildGeneratedSource{
		Target:          "drivers/tty/vt/consolemap_deftbl.c",
		Primary:         "drivers/tty/vt/cp437.uni",
		Prerequisites:   []string{"drivers/tty/vt/cp437.uni", "drivers/tty/vt/conmakehash"},
		CommandTemplate: "drivers/tty/vt/conmakehash $< > $@",
		CommandName:     "conmk",
		Directory:       "drivers/tty/vt",
	})

	// The recipe-less rule must not claim defkeymap.o.
	if _, ok, err := generatedSourceForTest(t, files, "drivers/tty/vt/defkeymap.o"); err != nil || ok {
		t.Fatalf("ForObject(defkeymap.o) = (ok=%v, err=%v), want no generated source", ok, err)
	}
}

// TestGeneratedSourceForObjectStaticPatternRule pins that 6.12's powerpc
// crypto shape parses into a bounded rule rather than a bogus prerequisite,
// and resolves through the static pattern.
func TestGeneratedSourceForObjectStaticPatternRule(t *testing.T) {
	got, ok, err := generatedSourceForTest(t, map[string]string{
		"Makefile": rootMakefileDescending("arch/powerpc/crypto"),
		"arch/powerpc/crypto/Makefile": `obj-y += aesp10-ppc.o ghashp10-ppc.o

quiet_cmd_perl = PERL    $@
      cmd_perl = $(PERL) $< > $@

targets += aesp10-ppc.S ghashp10-ppc.S

$(obj)/aesp10-ppc.S $(obj)/ghashp10-ppc.S: $(obj)/%.S: $(src)/%.pl FORCE
	$(call if_changed,perl)
`,
		"arch/powerpc/crypto/aesp10-ppc.pl":   "# perlasm\n",
		"arch/powerpc/crypto/ghashp10-ppc.pl": "# perlasm\n",
	}, "arch/powerpc/crypto/aesp10-ppc.o")
	if err != nil {
		t.Fatalf("ForObject() failed: %v", err)
	}
	if !ok {
		t.Fatalf("ForObject() found no generated source")
	}
	assertGeneratedSource(t, got, KbuildGeneratedSource{
		Target:          "arch/powerpc/crypto/aesp10-ppc.S",
		Stem:            "aesp10-ppc",
		Primary:         "arch/powerpc/crypto/aesp10-ppc.pl",
		Prerequisites:   []string{"arch/powerpc/crypto/aesp10-ppc.pl"},
		CommandTemplate: "$(PERL) $< > $@",
		CommandName:     "perl",
		Directory:       "arch/powerpc/crypto",
	})
}

// TestGeneratedSourceForObjectPrefersCSourceOverAssembly pins the candidate
// order when one stem can be produced by rules for two compiled extensions.
//
// scripts/Makefile.build defines "$(obj)/%.o: $(obj)/%.c" before
// "$(obj)/%.o: $(obj)/%.S", and make tries pattern rules in definition order,
// so the C source wins. The on-disk probe in sourceForObject resolves the same
// way, and the two must agree or an object's source depends on which path
// happened to answer.
func TestGeneratedSourceForObjectPrefersCSourceOverAssembly(t *testing.T) {
	got, ok, err := generatedSourceForTest(t, map[string]string{
		"Makefile": rootMakefileDescending("lib/demo"),
		"lib/demo/Makefile": `obj-y += both.o

quiet_cmd_gencsource = GENC    $@
      cmd_gencsource = $(PERL) $< > $@
quiet_cmd_genasm = GENASM  $@
      cmd_genasm = $(PERL) $< > $@

$(obj)/%.S: $(src)/%.pl FORCE
	$(call if_changed,genasm)
$(obj)/%.c: $(src)/%.tmpl FORCE
	$(call if_changed,gencsource)
`,
		"lib/demo/both.pl":   "# perlasm\n",
		"lib/demo/both.tmpl": "/* template */\n",
	}, "lib/demo/both.o")
	if err != nil {
		t.Fatalf("ForObject() failed: %v", err)
	}
	if !ok {
		t.Fatalf("ForObject() found no generated source")
	}
	// The ".S" rule is written first in the Makefile and would win on a
	// first-rule-seen scan; the candidate extension order is what makes ".c"
	// the answer.
	assertGeneratedSource(t, got, KbuildGeneratedSource{
		Target:          "lib/demo/both.c",
		Stem:            "both",
		Primary:         "lib/demo/both.tmpl",
		Prerequisites:   []string{"lib/demo/both.tmpl"},
		CommandTemplate: "$(PERL) $< > $@",
		CommandName:     "gencsource",
		Directory:       "lib/demo",
	})
}

// TestGeneratedSourceForObjectIgnoresUnmatchedObjects pins that a
// directory-wide "%.S: %.pl" rule does not claim every object in the
// directory: a pattern rule only applies when its prerequisites exist.
func TestGeneratedSourceForObjectIgnoresUnmatchedObjects(t *testing.T) {
	files := map[string]string{
		"Makefile": rootMakefileDescending("arch/x86/crypto"),
		"arch/x86/crypto/Makefile": `obj-y += aesni-intel.o sha256_ssse3.o

quiet_cmd_perlasm = PERLASM $@
      cmd_perlasm = $(PERL) $< > $@
$(obj)/%.S: $(src)/%.pl FORCE
	$(call if_changed,perlasm)
`,
		"arch/x86/crypto/aesni-intel.S":  "/* asm */\n",
		"arch/x86/crypto/sha256_ssse3.c": "/* c */\n",
	}
	for _, object := range []string{"arch/x86/crypto/aesni-intel.o", "arch/x86/crypto/sha256_ssse3.o"} {
		if _, ok, err := generatedSourceForTest(t, files, object); err != nil || ok {
			t.Errorf("ForObject(%q) = (ok=%v, err=%v), want no generated source", object, ok, err)
		}
	}
}

// TestGeneratedSourceForObjectIgnoresInactiveRules confirms that a rule inside
// an inactive conditional is never captured, so KbuildRule needs no condition
// of its own. drivers/tty/vt's GENERATE_KEYMAP block is the real instance.
func TestGeneratedSourceForObjectIgnoresInactiveRules(t *testing.T) {
	_, ok, err := generatedSourceForTest(t, map[string]string{
		"Makefile": rootMakefileDescending("drivers/tty/vt"),
		"drivers/tty/vt/Makefile": `obj-y += defkeymap.o

ifdef GENERATE_KEYMAP
$(obj)/defkeymap.c: $(obj)/%.c: $(src)/%.map
	loadkeys --mktable --unicode $< > $@
endif
`,
		"drivers/tty/vt/defkeymap.map": "keymap\n",
	}, "drivers/tty/vt/defkeymap.o")
	if err != nil || ok {
		t.Fatalf("ForObject() = (ok=%v, err=%v), want no generated source", ok, err)
	}
}

// TestGeneratedSourceForObjectFailsOnErasedReference pins the fail-closed
// behaviour for a cmd_* macro argument that expanded to nothing. Silently
// dropping it would turn a three-argument invocation into a two-argument one,
// which is a wrong answer rather than a failure.
func TestGeneratedSourceForObjectFailsOnErasedReference(t *testing.T) {
	_, _, err := generatedSourceForTest(t, map[string]string{
		"Makefile": rootMakefileDescending("lib/crypto"),
		"lib/crypto/Makefile": `obj-y += poly1305-core.o

poly1305-perlasm-flavour-y :=
poly1305-perlasm-flavour-n := 64

quiet_cmd_perlasm_poly1305 = PERLASM $@
      cmd_perlasm_poly1305 = $(PERL) $< $(poly1305-perlasm-flavour-y) $@
$(obj)/poly1305-core.S: $(src)/poly1305-riscv.pl FORCE
	$(call if_changed,perlasm_poly1305)
`,
		"lib/crypto/poly1305-riscv.pl": "# perlasm\n",
	}, "lib/crypto/poly1305-core.o")
	if err == nil {
		t.Fatalf("ForObject() succeeded, want an error about the erased reference")
	}
	if !strings.Contains(err.Error(), "expanded to nothing") {
		t.Fatalf("ForObject() error = %v, want it to name the erased expansion", err)
	}
}

// TestGeneratedSourceForObjectFailsOnAmbiguousRules pins that equally good
// pattern rules are reported rather than silently resolved one way.
func TestGeneratedSourceForObjectFailsOnAmbiguousRules(t *testing.T) {
	_, _, err := generatedSourceForTest(t, map[string]string{
		"Makefile": rootMakefileDescending("drivers/thing"),
		"drivers/thing/Makefile": `obj-y += widget.o

quiet_cmd_one = ONE $@
      cmd_one = $(PERL) $< > $@
quiet_cmd_two = TWO $@
      cmd_two = $(PERL) $< > $@

$(obj)/%.c: $(src)/%.first FORCE
	$(call if_changed,one)
$(obj)/%.c: $(src)/%.second FORCE
	$(call if_changed,two)
`,
		"drivers/thing/widget.first":  "a\n",
		"drivers/thing/widget.second": "b\n",
	}, "drivers/thing/widget.o")
	if err == nil {
		t.Fatalf("ForObject() succeeded, want an ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ForObject() error = %v, want it to report ambiguity", err)
	}
}

func assertGeneratedSource(t *testing.T, got, want KbuildGeneratedSource) {
	t.Helper()
	// Position is a diagnostic detail, compared only for being populated.
	if got.Position.Line == 0 {
		t.Errorf("generated source has no rule position")
	}
	got.Position = Position{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("generated source mismatch\nwant: %#v\n got: %#v", want, got)
	}
}
