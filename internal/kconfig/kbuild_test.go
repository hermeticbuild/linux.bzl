package kconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestParseKbuildCommonObjectPatterns(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`obj-y += init/main.o generated.c ignored/
obj-m += module.o
obj-$(CONFIG_NET) += net/core.o
obj-${CONFIG_USB} += usb/core.o
lib-y += lib/string.o
lib-m += lib/crc.o
lib-$(CONFIG_CRYPTO) += lib/crypto.o
obj-y += drivers/
obj-$(CONFIG_SOUND) += sound/
subdir-y += tools
subdir-$(CONFIG_DTB) += dts
core-y += kernel/
drivers-$(CONFIG_PCI) += arch/x86/pci/
obj-y += $(generated-y) $(obj)/dynamic.o generated.h
always-y += always.o
targets += target.o
hostprogs-y += host-helper.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "init/main.o", kind: "const", state: "y", line: 1},
		{object: "module.o", kind: "const", state: "m", line: 2},
		{object: "net/core.o", kind: "config", symbol: "CONFIG_NET", line: 3},
		{object: "usb/core.o", kind: "config", symbol: "CONFIG_USB", line: 4},
		{object: "lib/string.o", kind: "const", state: "y", line: 5},
		{object: "lib/crc.o", kind: "const", state: "m", line: 6},
		{object: "lib/crypto.o", kind: "config", symbol: "CONFIG_CRYPTO", line: 7},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}

	gotDirs := kbuildDirSummaries(kb.Directories)
	wantDirs := []kbuildDirSummary{
		{kind: "obj", directory: "ignored/", condKind: "const", state: "y", line: 1},
		{kind: "obj", directory: "drivers/", condKind: "const", state: "y", line: 8},
		{kind: "obj", directory: "sound/", condKind: "config", symbol: "CONFIG_SOUND", line: 9},
		{kind: "subdir", directory: "tools/", condKind: "const", state: "y", line: 10},
		{kind: "subdir", directory: "dts/", condKind: "config", symbol: "CONFIG_DTB", line: 11},
		{kind: "core", directory: "kernel/", condKind: "const", state: "y", line: 12},
		{kind: "drivers", directory: "arch/x86/pci/", condKind: "config", symbol: "CONFIG_PCI", line: 13},
	}
	if !reflect.DeepEqual(gotDirs, wantDirs) {
		t.Fatalf("directories mismatch\nwant: %#v\n got: %#v", wantDirs, gotDirs)
	}
}

func TestParseKbuildExpandsMakeVariablesAndFunctions(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`objects := core/main.o generated.h
subdirs := drivers/net firmware
obj-y += $(filter %.o,$(objects)) $(addsuffix /,$(filter drivers/%,$(subdirs)))
targets += $(patsubst %.c,%.o,foo.c bar.S)
CFLAGS_core/main.o += $(filter-out -Wbad,$(sort -Wok -Wbad -Wok))
targets += $(findstring needle,hay needle stack).o $(firstword alpha beta).o $(lastword alpha beta).o word$(word 2,one two three).o count$(words one two three).o
obj-y += $(notdir $(lastword $(MAKEFILE_LIST))).o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	gotObjects := kbuildObjectSummaries(kb.Objects)
	wantObjects := []kbuildObjectSummary{
		{object: "core/main.o", kind: "const", state: "y", line: 3},
		{object: "Kbuild.o", kind: "const", state: "y", line: 7},
	}
	if !reflect.DeepEqual(gotObjects, wantObjects) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", wantObjects, gotObjects)
	}

	gotDirs := kbuildDirSummaries(kb.Directories)
	wantDirs := []kbuildDirSummary{
		{kind: "obj", directory: "drivers/net/", condKind: "const", state: "y", line: 3},
	}
	if !reflect.DeepEqual(gotDirs, wantDirs) {
		t.Fatalf("directories mismatch\nwant: %#v\n got: %#v", wantDirs, gotDirs)
	}

	gotGenerated := kbuildGeneratedSummaries(kb.Generated)
	wantGenerated := []kbuildGeneratedSummary{
		{kind: "targets", target: "foo.o", condKind: "const", state: "y", line: 4},
		{kind: "targets", target: "bar.S", condKind: "const", state: "y", line: 4},
		{kind: "targets", target: "needle.o", condKind: "const", state: "y", line: 6},
		{kind: "targets", target: "alpha.o", condKind: "const", state: "y", line: 6},
		{kind: "targets", target: "beta.o", condKind: "const", state: "y", line: 6},
		{kind: "targets", target: "wordtwo.o", condKind: "const", state: "y", line: 6},
		{kind: "targets", target: "count3.o", condKind: "const", state: "y", line: 6},
	}
	if !reflect.DeepEqual(gotGenerated, wantGenerated) {
		t.Fatalf("generated mismatch\nwant: %#v\n got: %#v", wantGenerated, gotGenerated)
	}

	gotFlags := kbuildFlagSummaries(kb.Flags)
	wantFlags := []kbuildFlagSummary{
		{scope: "object", object: "core/main.o", flags: "-Wok", kind: "const", state: "y", line: 5},
	}
	if !reflect.DeepEqual(gotFlags, wantFlags) {
		t.Fatalf("flags mismatch\nwant: %#v\n got: %#v", wantFlags, gotFlags)
	}
}

func TestParseKbuildCapturesSanitizerObjectSettings(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`KASAN_SANITIZE := n
KASAN_SANITIZE_head$(BITS).o += n
KCSAN_SANITIZE_racy.o := y
KCSAN_INSTRUMENT_BARRIERS_racy.o := y
UBSAN_SANITIZE_undefined.o := n
UBSAN_SIGNED_WRAP_legacy.o := y
UBSAN_INTEGER_WRAP_modern.o := y
CFLAGS_KASAN_TEST := $(CFLAGS_KASAN)
CFLAGS_kasan_test_c.o := $(CFLAGS_KASAN_TEST) -fno-builtin
CFLAGS_kcsan_test.o := $(CFLAGS_KCSAN) -fno-omit-frame-pointer
UNRELATED_SETTING_ignored.o := y
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{"BITS": "64"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}

	want := []kbuildObjectSetting{
		{Name: "KASAN_SANITIZE", Value: "n"},
		{Name: "KASAN_SANITIZE", Object: "head64.o", Value: "n"},
		{Name: "KCSAN_INSTRUMENT_BARRIERS", Object: "racy.o", Value: "y"},
		{Name: "KCSAN_SANITIZE", Object: "racy.o", Value: "y"},
		{Name: "UBSAN_INTEGER_WRAP", Object: "modern.o", Value: "y"},
		{Name: "UBSAN_SANITIZE", Object: "undefined.o", Value: "n"},
		{Name: "UBSAN_SIGNED_WRAP", Object: "legacy.o", Value: "y"},
		{Name: "CFLAGS_KASAN", Object: "kasan_test_c.o", Value: "y"},
		{Name: "CFLAGS_KCSAN", Object: "kcsan_test.o", Value: "y"},
	}
	if !reflect.DeepEqual(kb.objectSettings, want) {
		t.Fatalf("object settings mismatch\nwant: %#v\n got: %#v", want, kb.objectSettings)
	}
	gotFlags := map[string][]string{}
	for _, flag := range kb.Flags {
		if flag.Scope == "object" {
			gotFlags[flag.Object] = append(gotFlags[flag.Object], flag.Flags...)
		}
	}
	wantFlags := map[string][]string{
		"kasan_test_c.o": {"-fno-builtin"},
		"kcsan_test.o":   {"-fno-omit-frame-pointer"},
	}
	if !reflect.DeepEqual(gotFlags, wantFlags) {
		t.Fatalf("explicit sanitizer reference filtering mismatch\nwant: %#v\n got: %#v", wantFlags, gotFlags)
	}
}

func TestKbuildClangProbeModelsKcsanRuntimeOptionsByArchitecture(t *testing.T) {
	for _, test := range []struct {
		srcarch string
		want    []string
	}{
		{srcarch: "x86", want: []string{"-fno-stack-protector"}},
		{srcarch: "arm64", want: []string{"-mno-outline-atomics", "-fno-stack-protector"}},
	} {
		t.Run(test.srcarch, func(t *testing.T) {
			dir := t.TempDir()
			kbuild := filepath.Join(dir, "Kbuild")
			if err := os.WriteFile(kbuild, []byte(`obj-y += core.o
CFLAGS_core.o := $(call cc-option,-fno-conserve-stack) \
	$(call cc-option,-mno-outline-atomics) -fno-stack-protector
`), 0o644); err != nil {
				t.Fatalf("WriteFile(Kbuild) failed: %v", err)
			}
			kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
				Variables: map[string]string{"SRCARCH": test.srcarch},
			})
			if err != nil {
				t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
			}
			var got []string
			for _, flag := range kb.Flags {
				if flag.Scope == "object" && flag.Object == "core.o" {
					got = append(got, flag.Flags...)
				}
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("KCSAN runtime flags = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestKbuildClangCapabilityPolicyResolvesKnownCallsEagerly(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`comma := ,
obj-y += core.o
CFLAGS_core.o := $(call cc-option,-Wno-gnu) \
	$(call cc-option,-Wno-psabi) \
	$(call cc-option,-mabi=lp64) \
	$(call cc-option,-mbranch-protection=none) \
	$(call cc-option,-Wmaybe-uninitialized,-Wno-uninitialized) \
	$(call as-option,-Wa$(comma)-march=armv8.5-a) \
	$(call ld-option,-maarch64elf)
obj-$(call cc-option-yn,-Wno-vla) += supported.o
obj-$(call cc-option-yn,-Wrestrict) += unsupported.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{"SRCARCH": "arm64"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	var gotFlags []string
	for _, flag := range kb.Flags {
		if flag.Scope == "object" && flag.Object == "core.o" {
			gotFlags = append(gotFlags, flag.Flags...)
		}
	}
	wantFlags := []string{
		"-Wno-gnu",
		"-Wno-psabi",
		"-mbranch-protection=none",
		"-Wno-uninitialized",
		"-Wa,-march=armv8.5-a",
		"-maarch64elf",
	}
	if !reflect.DeepEqual(gotFlags, wantFlags) {
		t.Fatalf("resolved Clang flags = %#v, want %#v", gotFlags, wantFlags)
	}
	gotObjects := kbuildObjectSummaries(kb.Objects)
	if !slices.ContainsFunc(gotObjects, func(object kbuildObjectSummary) bool {
		return object.object == "supported.o" && object.state == "y"
	}) {
		t.Fatalf("known supported cc-option-yn object missing from %#v", gotObjects)
	}
	if slices.ContainsFunc(gotObjects, func(object kbuildObjectSummary) bool {
		return object.object == "unsupported.o"
	}) {
		t.Fatalf("known unsupported cc-option-yn object unexpectedly present in %#v", gotObjects)
	}
}

func TestKbuildClangCapabilityPolicyUsesRelevantContext(t *testing.T) {
	for _, test := range []struct {
		name    string
		context string
		want    string
	}{
		{
			name:    "supported stack alignment context",
			context: "-m32 -mstack-alignment=4",
			want:    "-march=atom",
		},
		{
			name:    "unsupported preferred boundary context",
			context: "-m32 -mpreferred-stack-boundary=2",
			want:    "-march=i386",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			kbuild := filepath.Join(dir, "Kbuild")
			content := "obj-y += core.o\n" +
				"KBUILD_CFLAGS := " + test.context + "\n" +
				"CFLAGS_core.o := $(call cc-option,-march=atom,-march=i386)\n"
			if err := os.WriteFile(kbuild, []byte(content), 0o644); err != nil {
				t.Fatalf("WriteFile(Kbuild) failed: %v", err)
			}
			kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
				Variables: map[string]string{"SRCARCH": "x86"},
			})
			if err != nil {
				t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
			}
			var got []string
			for _, flag := range kb.Flags {
				if flag.Scope == "object" && flag.Object == "core.o" {
					got = append(got, flag.Flags...)
				}
			}
			if !reflect.DeepEqual(got, []string{test.want}) {
				t.Fatalf("contextual cc-option flags = %#v, want %q", got, test.want)
			}
		})
	}
}

func TestKbuildClangCapabilityPolicyResolvesLinuxX8664MakefileOptions(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`cc_stack_align4 := -mstack-alignment=4
obj-y += core.o
CFLAGS_core.o := $(call cc-option,-falign-jumps=1) \
	$(call cc-option,-falign-loops=1) \
	$(call cc-option,-mno-fp-ret-in-387) \
	$(call cc-option,-mskip-rax-setup)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{
			"KBUILD_CFLAGS": "-mno-sse -mno-mmx -mno-sse2 -mno-3dnow -mno-avx -mno-sse4a $(call cc-option,-fcf-protection=branch -fno-jump-tables) $(call cc-option,-fcf-protection=none) -m32 -msoft-float -mregparm=3 -freg-struct-return -fno-pic $(cc_stack_align4) $(cflags-y) -ffreestanding -m64",
			"SRCARCH":       "x86",
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	var got []string
	for _, flag := range kb.Flags {
		if flag.Scope == "object" && flag.Object == "core.o" {
			got = append(got, flag.Flags...)
		}
	}
	want := []string{"-falign-loops=1", "-mno-fp-ret-in-387", "-mskip-rax-setup"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("x86_64 Makefile flags = %#v, want %#v", got, want)
	}
}

func TestKbuildClangCapabilityPolicyResolvesLinux618X8664MakefileOptions(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(kbuild, []byte(`obj-y += core.o
CFLAGS_core.o := $(call cc-option,-Wa$(comma)-mtune=generic32,) \
	$(call cc-option,-maccumulate-outgoing-args,)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Makefile) failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{
			"KBUILD_CFLAGS": "-m64",
			"SRCARCH":       "x86",
			"comma":         ",",
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	for _, flag := range kb.Flags {
		if flag.Scope == "object" && flag.Object == "core.o" && len(flag.Flags) != 0 {
			t.Fatalf("unsupported x86_64 Clang flags unexpectedly emitted: %#v", flag.Flags)
		}
	}
}

func TestKbuildClangCapabilityPolicyResolvesLinux618X8664SubtreeOptions(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(kbuild, []byte(`obj-y += core.o
CFLAGS_core.o := $(call cc-option,-Wa$(comma)-mrelax-relocations=no) \
	$(call ld-option,--eh-frame-hdr)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Makefile) failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{
			"KBUILD_CFLAGS":  "-m64",
			"KBUILD_LDFLAGS": "-m elf_x86_64",
			"SRCARCH":        "x86",
			"comma":          ",",
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	var got []string
	for _, flag := range kb.Flags {
		if flag.Scope == "object" && flag.Object == "core.o" {
			got = append(got, flag.Flags...)
		}
	}
	want := []string{"-Wa,-mrelax-relocations=no", "--eh-frame-hdr"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved x86_64 subtree flags = %#v, want %#v", got, want)
	}
}

func TestKbuildClangCapabilityPolicyResolvesLinux618SharedTreeOptions(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(kbuild, []byte(`obj-y += core.o
CFLAGS_core.o := $(call cc-option,-fno-schedule-insns) \
	$(call cc-option,-fsched-pressure) \
	$(call cc-option,-femit-struct-debug-detailed=any) \
	$(call cc-disable-warning,stringop-truncation) \
	$(call cc-option,-fno-addrsig) \
	$(call cc-option,-Wold-style-declaration,-Wout-of-line-declaration) \
	$(call cc-option,-mgeneral-regs-only) \
	$(call cc-disable-warning,psabi) \
	$(call cc-disable-warning,unused-but-set-variable) \
	$(call cc-disable-warning,unused-const-variable) \
	$(call cc-disable-warning,fortify-source) \
	$(call cc-disable-warning,unsequenced) \
	$(call cc-option,-Wvla-larger-than=1) \
	$(call cc-disable-warning,uninitialized) \
	$(call cc-disable-warning,missing-prototypes) \
	$(call cc-disable-warning,stringop-overread) \
	$(call cc-disable-warning,switch-unreachable) \
	$(call cc-disable-warning,tautological-constant-out-of-range-compare)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Makefile) failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{
			"KBUILD_CFLAGS": "-m64 -fintegrated-as",
			"SRCARCH":       "x86",
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	var got []string
	for _, flag := range kb.Flags {
		if flag.Scope == "object" && flag.Object == "core.o" {
			got = append(got, flag.Flags...)
		}
	}
	want := []string{
		"-fno-addrsig",
		"-Wout-of-line-declaration",
		"-mgeneral-regs-only",
		"-Wno-psabi",
		"-Wno-unused-but-set-variable",
		"-Wno-unused-const-variable",
		"-Wno-fortify-source",
		"-Wno-unsequenced",
		"-Wno-uninitialized",
		"-Wno-missing-prototypes",
		"-Wno-tautological-constant-out-of-range-compare",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved shared-tree flags = %#v, want %#v", got, want)
	}
}

func TestKbuildClangCapabilityPolicyResolvesDynamicMacroPrefixMap(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(kbuild, []byte(`obj-y += core.o
CFLAGS_core.o := $(call cc-option,-fmacro-prefix-map=$(srctree)/=)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Makefile) failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{
			"KBUILD_CFLAGS": "-m64",
			"SRCARCH":       "x86",
			"srctree":       dir,
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	var got []string
	for _, flag := range kb.Flags {
		if flag.Scope == "object" && flag.Object == "core.o" {
			got = append(got, flag.Flags...)
		}
	}
	want := []string{"-fmacro-prefix-map=" + dir + "/="}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dynamic macro prefix-map flags = %#v, want %#v", got, want)
	}
}

func TestKbuildClangCapabilityPolicyDefersRecursiveCallArguments(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`tune = $(call cc-option,-mtune=$(1),$(2))
cc_stack_align4 := -mstack-alignment=4
KBUILD_CFLAGS += $(cc_stack_align4)
obj-y += core.o
CFLAGS_core.o := $(call tune,pentium4)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{"SRCARCH": "x86"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	var got []string
	for _, flag := range kb.Flags {
		if flag.Scope == "object" && flag.Object == "core.o" {
			got = append(got, flag.Flags...)
		}
	}
	if !reflect.DeepEqual(got, []string{"-mtune=pentium4"}) {
		t.Fatalf("recursive capability-call flags = %#v, want -mtune=pentium4", got)
	}
}

func TestKbuildClangCapabilityPolicyResolvesNestedTuneFallback(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`cc_stack_align4 := -mstack-alignment=4
tune = $(call cc-option,-mtune=$(1),$(2))
tune-i686 = $(call tune,i686,$(call tune,generic))
KBUILD_CFLAGS += $(cc_stack_align4)
obj-y += core.o
CFLAGS_core.o := $(call tune-i686)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{"SRCARCH": "x86"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	var got []string
	for _, flag := range kb.Flags {
		if flag.Scope == "object" && flag.Object == "core.o" {
			got = append(got, flag.Flags...)
		}
	}
	if !reflect.DeepEqual(got, []string{"-mtune=i686"}) {
		t.Fatalf("nested capability-call flags = %#v, want -mtune=i686", got)
	}
}

func TestKbuildClangCapabilityPolicyRejectsUnknownCandidate(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`obj-y += core.o
CFLAGS_core.o := $(call cc-option,-fbrand-new-kernel-flag)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	_, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{
			"KBUILD_CFLAGS": "-m64",
			"SRCARCH":       "x86",
		},
	})
	if err == nil {
		t.Fatal("ParseKbuildFileWithOptions() unexpectedly succeeded")
	}
	for _, want := range []string{
		"Kbuild:2",
		"-fbrand-new-kernel-flag",
		`architecture "x86_64"`,
		`context "-Werror -m64"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("unknown-candidate error %q does not contain %q", err, want)
		}
	}
}

func TestKbuildClangCapabilityPolicyRejectsUnresolvedNonPositionalCandidate(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`obj-y += core.o
CFLAGS_core.o := $(call cc-option,$(UNKNOWN_COMPILER_FLAG))
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	_, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{
			"KBUILD_CFLAGS": "-m64",
			"SRCARCH":       "x86",
		},
	})
	if err == nil {
		t.Fatal("ParseKbuildFileWithOptions() unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "UNKNOWN_COMPILER_FLAG") {
		t.Fatalf("unresolved-candidate error %q does not identify the unresolved variable", err)
	}
}

func TestParseKbuildDirectoryTreePrefixesSanitizerObjectSettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`obj-y += root.o child/
KASAN_SANITIZE_root.o := n
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "child"), 0o755); err != nil {
		t.Fatalf("Mkdir(child) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child", "Makefile"), []byte(`obj-y += default.o override.o
KASAN_SANITIZE := n
KASAN_SANITIZE_override.o := y
KCSAN_INSTRUMENT_BARRIERS := y
`), 0o644); err != nil {
		t.Fatalf("WriteFile(child/Makefile) failed: %v", err)
	}
	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{RootDir: dir})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}

	want := []kbuildObjectSetting{
		{Name: "KASAN_SANITIZE", Object: "root.o", Value: "n"},
		{Name: "KASAN_SANITIZE", Directory: "child", Value: "n"},
		{Name: "KASAN_SANITIZE", Object: "child/override.o", Directory: "child", Value: "y"},
		{Name: "KCSAN_INSTRUMENT_BARRIERS", Directory: "child", Value: "y"},
	}
	if !reflect.DeepEqual(kb.objectSettings, want) {
		t.Fatalf("prefixed object settings mismatch\nwant: %#v\n got: %#v", want, kb.objectSettings)
	}
}

func TestParseKbuildDirectoryTreePropagatesProbeOptionToRootMakefiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte("obj-y += root.o child/\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	childDir := filepath.Join(dir, "child")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(child) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "Makefile"), []byte(`obj-y += child.o
CFLAGS_child.o := $(call cc-option,-fchild-probe)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(child/Makefile) failed: %v", err)
	}
	archDir := filepath.Join(dir, "arch", "arm")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(arch/arm) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archDir, "Makefile"), []byte(`obj-y += arch.o
CFLAGS_arch.o := $(call cc-option,-fno-dwarf2-cfi-asm)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(arch/arm/Makefile) failed: %v", err)
	}

	probeCalls := map[string]int{}
	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{
		RootDir:       dir,
		RootMakefiles: []string{"arch/arm/Makefile"},
		Variables:     map[string]string{"SRCARCH": "arm"},
		ProbeOption: func(kind string, candidate, context []string) (bool, error) {
			if kind != "cc_option" || len(candidate) != 1 {
				t.Fatalf("probe = %q, %#v; want one cc-option candidate", kind, candidate)
			}
			probeCalls[candidate[0]]++
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	wantProbeCalls := map[string]int{"-fchild-probe": 1, "-fno-dwarf2-cfi-asm": 1}
	if !reflect.DeepEqual(probeCalls, wantProbeCalls) {
		t.Fatalf("ProbeOption calls = %#v, want %#v", probeCalls, wantProbeCalls)
	}
	got := map[string][]string{}
	for _, flag := range kb.Flags {
		if flag.Scope == "object" {
			got[flag.Object] = append(got[flag.Object], flag.Flags...)
		}
	}
	want := map[string][]string{
		"arch.o":        {"-fno-dwarf2-cfi-asm"},
		"child/child.o": {"-fchild-probe"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("probed directory-tree flags = %#v, want %#v", got, want)
	}
}

func TestMeasuredKbuildProbeContextUsesEarlierConcreteResults(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(kbuild, []byte(`KBUILD_CFLAGS += $(call cc-option,-fno-dwarf2-cfi-asm)
KBUILD_CFLAGS += $(call cc-option,-mno-fdpic)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Makefile) failed: %v", err)
	}

	var contexts [][]string
	_, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{"SRCARCH": "arm"},
		ProbeOption: func(kind string, candidate, context []string) (bool, error) {
			if kind != "cc_option" || len(candidate) != 1 {
				t.Fatalf("probe = %q, %#v; want one cc-option candidate", kind, candidate)
			}
			contexts = append(contexts, append([]string(nil), context...))
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	want := [][]string{
		{"-Werror"},
		{"-Werror", "-fno-dwarf2-cfi-asm"},
	}
	if !reflect.DeepEqual(contexts, want) {
		t.Fatalf("probe contexts = %#v, want %#v", contexts, want)
	}
}

func TestMeasuredKbuildProbeContextDropsWholeUnresolvedTryRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(path, []byte(`cc_has_k_constraint := $(call try-run,echo \
	'int main(void) { \
		asm volatile("and w0, w0, %w0" :: "K" (4294967295)); \
		return 0; \
	}' | $(CC) -S -x c -o "$$TMP" -,,-DCONFIG_CC_HAS_K_CONSTRAINT=1)
KBUILD_CFLAGS += -mgeneral-regs-only $(cc_has_k_constraint)
KBUILD_CFLAGS += $(call cc-option,-mabi=lp64)
`), 0o644); err != nil {
		t.Fatal(err)
	}

	var got []string
	_, err := ParseKbuildFileWithOptions(path, KbuildOptions{
		Variables: map[string]string{"SRCARCH": "arm64"},
		ProbeOption: func(kind string, candidate, context []string) (bool, error) {
			if kind == "cc_option" && reflect.DeepEqual(candidate, []string{"-mabi=lp64"}) {
				got = append([]string(nil), context...)
			}
			return true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-Werror", "-mgeneral-regs-only"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cc-option context = %#v, want %#v", got, want)
	}
}

func TestMeasuredKbuildProbeContextExpandsMakeExpressions(t *testing.T) {
	for _, tc := range []struct {
		name             string
		content          string
		finalCandidate   string
		wantFinalContext []string
	}{
		{
			name: "riscv subst self reference",
			content: `CC_FLAGS_FTRACE := -pg
KBUILD_CFLAGS := $(subst $(CC_FLAGS_FTRACE),,$(KBUILD_CFLAGS)) -fpie $(call cc-option,-mbranch-protection=none)
KBUILD_CFLAGS += $(call cc-option,-fno-addrsig)
`,
			finalCandidate:   "-fno-addrsig",
			wantFinalContext: []string{"-Werror", "-fpie", "-mbranch-protection=none"},
		},
		{
			name: "powerpc recursive calls",
			content: `KBUILD_CFLAGS = $(call cc-option,-mno-sched-epilog)
CC_OPTION_CFLAGS = $(KBUILD_CFLAGS) $(call cc-option,-mno-string)
ccflags-y += $(call cc-option,-fno-stack-protector)
`,
			finalCandidate:   "-fno-stack-protector",
			wantFinalContext: []string{"-Werror", "-mno-sched-epilog", "-mno-string"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "Makefile")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			var finalContext []string
			_, err := ParseKbuildFileWithOptions(path, KbuildOptions{
				Variables: map[string]string{"SRCARCH": "riscv"},
				ProbeOption: func(kind string, candidate, context []string) (bool, error) {
					for _, arg := range context {
						if containsMakeReference(arg) {
							t.Fatalf("probe %q retained make expression in context %q", candidate, context)
						}
					}
					if len(candidate) == 1 && candidate[0] == tc.finalCandidate {
						finalContext = append([]string(nil), context...)
					}
					return true, nil
				},
			})
			if err != nil {
				t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
			}
			if !reflect.DeepEqual(finalContext, tc.wantFinalContext) {
				t.Fatalf("final probe context = %#v, want %#v", finalContext, tc.wantFinalContext)
			}
		})
	}
}

func TestMeasuredKbuildAsInstrUsesSourceProbeAndConcreteResult(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte("obj-y += root.o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archDir := filepath.Join(dir, "arch", "powerpc")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archDir, "Makefile"), []byte(`comma := ,
CLANG_FLAGS := -fintegrated-as
KBUILD_AFLAGS := -m64 -I /kernel/arch/powerpc
asinstr := $(call as-instr,lis 9$(comma)foo@high,-DHAVE_AS_ATHIGH=1)
KBUILD_CPPFLAGS += $(asinstr)
KBUILD_CFLAGS += $(call cc-option,-mno-sched-epilog)
`), 0o644); err != nil {
		t.Fatal(err)
	}

	var sourceCalls int
	var optionContext []string
	_, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{
		RootDir:       dir,
		RootMakefiles: []string{"arch/powerpc/Makefile"},
		Variables:     map[string]string{"SRCARCH": "powerpc"},
		ProbeSource: func(language, source string, context []string) (bool, error) {
			sourceCalls++
			if language != "assembler-with-cpp" || source != "lis 9,foo@high" {
				t.Fatalf("source probe = %q, %q; want PowerPC as-instr source", language, source)
			}
			wantContext := []string{"-fintegrated-as", "-m64", "-I", "/kernel/arch/powerpc"}
			if !reflect.DeepEqual(context, wantContext) {
				t.Fatalf("source probe context = %#v, want %#v", context, wantContext)
			}
			return true, nil
		},
		ProbeOption: func(kind string, candidate, context []string) (bool, error) {
			if kind == "cc_option" && reflect.DeepEqual(candidate, []string{"-mno-sched-epilog"}) {
				optionContext = append([]string(nil), context...)
			}
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	if sourceCalls != 1 {
		t.Fatalf("source probe calls = %d, want 1", sourceCalls)
	}
	if !slices.Contains(optionContext, "-DHAVE_AS_ATHIGH=1") {
		t.Fatalf("later cc-option context = %#v, want measured as-instr result", optionContext)
	}
	for _, arg := range optionContext {
		if containsMakeReference(arg) || strings.Contains(arg, "as-instr") {
			t.Fatalf("later cc-option context retained raw as-instr expression: %#v", optionContext)
		}
	}
}

func TestParseKbuildExpandsConfigurationIndexedFlagFamilies(t *testing.T) {
	for _, test := range []struct {
		name   string
		thumb2 string
		want   []string
	}{
		{name: "disabled", thumb2: "", want: nil},
		{name: "enabled", thumb2: "y", want: []string{"-U__thumb2__", "-D__thumb2__=1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "Makefile")
			if err := os.WriteFile(path, []byte(`aflags-thumb2-$(CONFIG_THUMB2_KERNEL) := -U__thumb2__ -D__thumb2__=1
obj-y += arm/sha256-core.o other.o
AFLAGS_arm/sha256-core.o += $(aflags-thumb2-y)
AFLAGS_other.o += $(unrelated-y)
`), 0o644); err != nil {
				t.Fatal(err)
			}
			kb, err := ParseKbuildFileWithOptions(path, KbuildOptions{
				Variables: map[string]string{
					"CONFIG_THUMB2_KERNEL": test.thumb2,
					"SRCARCH":              "arm",
				},
			})
			if err != nil {
				t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
			}
			var got, unrelated []string
			for _, flag := range kb.Flags {
				switch flag.Object {
				case "arm/sha256-core.o":
					got = append(got, flag.Flags...)
				case "other.o":
					unrelated = append(unrelated, flag.Flags...)
				}
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Thumb-2 flags = %#v, want %#v", got, test.want)
			}
			if !reflect.DeepEqual(unrelated, []string{"$(unrelated-y)"}) {
				t.Fatalf("unrelated unknown family was silently erased: %#v", unrelated)
			}
		})
	}
}

func TestParseKbuildExpandsAdditionalPureMakeFunctions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.o"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile(existing.o) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "objects.list"), []byte("from-file.o\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(objects.list) failed: %v", err)
	}
	kbuildPath := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuildPath, []byte(`objects := one.o two.o three.o four.o
obj-y += $(wordlist 2,3,$(objects)) $(join joined-,one.o)
obj-y += $(notdir $(abspath rel.o)) $(notdir $(realpath existing.o)) $(notdir $(realpath missing.o))
ifeq ($(intcmp 1,0,,,y),y)
test-ge = $(intcmp $(strip $1)0,$(strip $2)0,,ge.o,ge.o)
test-gt = $(intcmp $(strip $1)0,$(strip $2)0,,,gt.o)
endif
obj-y += $(intcmp 1,1,lt.o,eq.o,gt.o) $(intcmp 0,1,lt.o,eq.o,gt.o) $(call test-ge,12,10) $(call test-gt,12,10)
obj-y += $(file < objects.list) $(file < missing.list)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}

	kb, err := ParseKbuildFile(kbuildPath)
	if err != nil {
		t.Fatalf("ParseKbuildFile() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "two.o", kind: "const", state: "y", line: 2},
		{object: "three.o", kind: "const", state: "y", line: 2},
		{object: "joined-one.o", kind: "const", state: "y", line: 2},
		{object: "rel.o", kind: "const", state: "y", line: 3},
		{object: "existing.o", kind: "const", state: "y", line: 3},
		{object: "eq.o", kind: "const", state: "y", line: 8},
		{object: "lt.o", kind: "const", state: "y", line: 8},
		{object: "ge.o", kind: "const", state: "y", line: 8},
		{object: "gt.o", kind: "const", state: "y", line: 8},
		{object: "from-file.o", kind: "const", state: "y", line: 9},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildEvaluatesLazyConditionalFunctions(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`enabled := y
disabled :=
obj-y += $(if $(enabled),if-then.o,$(unknown_if_then))
obj-y += $(if $(disabled),$(unknown_if_else),if-else.o)
obj-y += $(or or-first.o,$(unknown_or))
obj-y += $(and $(disabled),$(unknown_and))
obj-y += $(and $(enabled),and-last.o)
obj-y += $(if $(disabled),$(error inactive error branch),diagnostic-else.o)
obj-y += $(info parser note)$(warning parser warning)diagnostic.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "if-then.o", kind: "const", state: "y", line: 3},
		{object: "if-else.o", kind: "const", state: "y", line: 4},
		{object: "or-first.o", kind: "const", state: "y", line: 5},
		{object: "and-last.o", kind: "const", state: "y", line: 7},
		{object: "diagnostic-else.o", kind: "const", state: "y", line: 8},
		{object: "diagnostic.o", kind: "const", state: "y", line: 9},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}

	_, err = ParseKbuild(strings.NewReader(`$(error active failure)
`), "Kbuild")
	if err == nil {
		t.Fatalf("ParseKbuild() succeeded with active $(error)")
	}
}

func TestParseKbuildDefersErrorsInUnknownConditionalBranches(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`ifdef CONFIG_UNKNOWN
$(error config branch is only maybe active)
obj-y += maybe.o
else ifeq ($(unresolved),y)
$(error else-if branch is also maybe active)
obj-y += maybe-elseif.o
else
obj-y += fallback.o
endif
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "maybe.o", kind: "config_ne", symbol: "CONFIG_UNKNOWN", state: "n", line: 3},
		{object: "maybe-elseif.o", kind: "config_eq", symbol: "CONFIG_UNKNOWN", state: "n", line: 6},
		{object: "fallback.o", kind: "not", line: 8},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}

	_, err = ParseKbuild(strings.NewReader(`enabled := y
ifeq ($(enabled),y)
$(error known active failure)
endif
`), "Kbuild")
	if err == nil {
		t.Fatalf("ParseKbuild() succeeded with definitely active $(error)")
	}
}

func TestParseKbuildStripsOnlyTopLevelComments(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`obj-y += before.o # trailing comment.o
obj-y += $(shell grep -Ev '^#|^$$' params) after.o # another comment.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "before.o", kind: "const", state: "y", line: 1},
		{object: "after.o", kind: "const", state: "y", line: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildSplitsQuotedFlagWords(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`ccflags-y += -D'pr_fmt(fmt)=KBUILD_MODNAME ": " fmt' -DDEFAULT_SYMBOL_NAMESPACE='"USB_STORAGE"'
obj-y += test.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	if len(kb.Flags) != 1 {
		t.Fatalf("got %d flags entries, want 1", len(kb.Flags))
	}
	want := []string{
		`-Dpr_fmt(fmt)=KBUILD_MODNAME ": " fmt`,
		`-DDEFAULT_SYMBOL_NAMESPACE="USB_STORAGE"`,
	}
	if !reflect.DeepEqual(kb.Flags[0].Flags, want) {
		t.Fatalf("flags mismatch\nwant: %#v\n got: %#v", want, kb.Flags[0].Flags)
	}
}

func TestParseKbuildExpandsAssignmentLHSVariables(t *testing.T) {
	kb, err := parseKbuild(strings.NewReader(`obj-$(CONFIG_DRIVER) += driver.o
driver-$(CONFIG_MMU) := mmu.o
driver-y += always.o
`), "Kbuild", map[string]string{
		"CONFIG_DRIVER": "y",
		"CONFIG_MMU":    "y",
	}, "")
	if err != nil {
		t.Fatalf("parseKbuild() failed: %v", err)
	}

	gotObjects := kbuildObjectSummaries(kb.Objects)
	wantObjects := []kbuildObjectSummary{
		{object: "driver.o", kind: "config", symbol: "CONFIG_DRIVER", line: 1},
	}
	if !reflect.DeepEqual(gotObjects, wantObjects) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", wantObjects, gotObjects)
	}

	gotMembers := kbuildCompositeMemberSummaries(kb.compositeMembers)
	wantMembers := []kbuildCompositeMemberSummary{
		{composite: "driver.o", object: "mmu.o", kind: "config", symbol: "CONFIG_MMU", line: 2},
		{composite: "driver.o", object: "always.o", kind: "const", state: "y", line: 3},
	}
	if !reflect.DeepEqual(gotMembers, wantMembers) {
		t.Fatalf("members mismatch\nwant: %#v\n got: %#v", wantMembers, gotMembers)
	}
}

func TestKbuildConfigConditionsUseWrittenConfigState(t *testing.T) {
	kb, err := parseKbuild(strings.NewReader(`obj-$(CONFIG_HIDDEN) += hidden.o
obj-$(CONFIG_WRITTEN) += written.o
`), "Kbuild", map[string]string{
		"CONFIG_HIDDEN":  "y",
		"CONFIG_WRITTEN": "y",
	}, "")
	if err != nil {
		t.Fatalf("parseKbuild() failed: %v", err)
	}

	config := &ResolvedConfig{
		Effective: map[string]string{
			"CONFIG_HIDDEN":  "y",
			"CONFIG_WRITTEN": "y",
		},
		Written: map[string]bool{
			"CONFIG_WRITTEN": true,
		},
	}
	objects := kb.resolvedObjects(config)
	got := make([]string, 0, len(objects.roots))
	for _, object := range objects.roots {
		got = append(got, object.object)
	}
	want := []string{"written.o"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved roots mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildArm64NvheComposite(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`lib-objs := ../../../lib/memcpy.o
hyp-obj-y := switch.o ../entry.o $(lib-objs)
hyp-obj-$(CONFIG_LIST_HARDENED) += list_debug.o
obj-y := kvm_nvhe.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	wantMembers := []kbuildCompositeMemberSummary{
		{composite: "kvm_nvhe.o", object: "switch.nvhe.o", kind: "const", state: "y", line: 2},
		{composite: "kvm_nvhe.o", object: "../entry.nvhe.o", kind: "const", state: "y", line: 2},
		{composite: "kvm_nvhe.o", object: "../../../lib/memcpy.nvhe.o", kind: "const", state: "y", line: 2},
		{composite: "kvm_nvhe.o", object: "list_debug.nvhe.o", kind: "config", symbol: "CONFIG_LIST_HARDENED", line: 3},
	}
	gotMembers := kbuildCompositeMemberSummaries(kb.compositeMembers)
	for _, want := range wantMembers {
		if !slices.Contains(gotMembers, want) {
			t.Fatalf("members mismatch\nmissing: %#v\n got: %#v", want, gotMembers)
		}
	}
}

func TestParseKbuildExpandsCallAndForeachMacros(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`suffix-search = $(strip $(foreach s,$3,$($(1:%$(strip $2)=%$s))))
real-search = $(foreach m,$1,$(if $(call suffix-search,$m,$2,$3 -),$(call suffix-search,$m,$2,$3),$m))
objects := composite.o single.o
composite-y := core.o
composite-objs := base.o
obj-y += $(call real-search,$(objects),.o,-objs -y)
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "base.o", kind: "const", state: "y", line: 6},
		{object: "core.o", kind: "const", state: "y", line: 6},
		{object: "single.o", kind: "const", state: "y", line: 6},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildExpandsLetFunction(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`OUTPUT := global
$(let OUTPUT,$(OUTPUT)/,$(eval obj-y += $(OUTPUT)scoped.o))
obj-y += $(OUTPUT).o
obj-y += $(let first rest,one two three,$(first).o $(lastword $(rest)).o)
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "global/scoped.o", kind: "const", state: "y", line: 2},
		{object: "global.o", kind: "const", state: "y", line: 3},
		{object: "one.o", kind: "const", state: "y", line: 4},
		{object: "three.o", kind: "const", state: "y", line: 4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildPreservesRecursiveVariableReferences(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`recursive = $(recursive) hidden.o
obj-y += $(recursive) visible.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "hidden.o", kind: "const", state: "y", line: 2},
		{object: "visible.o", kind: "const", state: "y", line: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildHonorsMakeVariableFlavors(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`stem = before
recursive = $(stem)-recursive.o
simple := $(stem)-simple.o
stem = after
late_recursive := late-recursive.o
late_simple := late-simple.o
recursive += $(late_recursive)
simple += $(late_simple)
created += $(created_late)
created_late := created.o
maybe ?= maybe.o
maybe ?= ignored.o
obj-y += $(recursive) $(simple) $(created) $(maybe)
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "after-recursive.o", kind: "const", state: "y", line: 13},
		{object: "late-recursive.o", kind: "const", state: "y", line: 13},
		{object: "before-simple.o", kind: "const", state: "y", line: 13},
		{object: "late-simple.o", kind: "const", state: "y", line: 13},
		{object: "created.o", kind: "const", state: "y", line: 13},
		{object: "maybe.o", kind: "const", state: "y", line: 13},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildExpandsDefineMacros(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`define choose_objects
$(if $(1),defined.o,empty.o)
$(2)
endef
stem = early
define simple_object :=
$(stem)-simple.o
endef
stem = late
obj-y += $(call choose_objects,y,extra.o) $(call choose_objects,,fallback.o) $(simple_object)
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "defined.o", kind: "const", state: "y", line: 10},
		{object: "extra.o", kind: "const", state: "y", line: 10},
		{object: "empty.o", kind: "const", state: "y", line: 10},
		{object: "fallback.o", kind: "const", state: "y", line: 10},
		{object: "early-simple.o", kind: "const", state: "y", line: 10},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildEvaluatesEvalGeneratedAssignments(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`dynamic_targets_y += foo.o baz.o
dynamic_targets_m += bar.o qux.o
define WRAP_OBJ
wrapper-$(1)-y := $(1).o
obj-$(2) += wrapper-$(1).o
endef
$(foreach target,$(basename $(dynamic_targets_y)),$(eval $(call WRAP_OBJ,$(target),y)))
$(eval $(foreach target,$(basename $(dynamic_targets_m)),$(call WRAP_OBJ,$(target),m)))
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	gotObjects := kbuildObjectSummaries(kb.Objects)
	wantObjects := []kbuildObjectSummary{
		{object: "wrapper-foo.o", kind: "const", state: "y", line: 7},
		{object: "wrapper-baz.o", kind: "const", state: "y", line: 7},
		{object: "wrapper-bar.o", kind: "const", state: "m", line: 8},
		{object: "wrapper-qux.o", kind: "const", state: "m", line: 8},
	}
	if !reflect.DeepEqual(gotObjects, wantObjects) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", wantObjects, gotObjects)
	}

	gotMembers := kbuildCompositeMemberSummaries(kb.compositeMembers)
	wantMembers := []kbuildCompositeMemberSummary{
		{composite: "wrapper-foo.o", object: "foo.o", kind: "const", state: "y", line: 7},
		{composite: "wrapper-baz.o", object: "baz.o", kind: "const", state: "y", line: 7},
		{composite: "wrapper-bar.o", object: "bar.o", kind: "const", state: "y", line: 8},
		{composite: "wrapper-qux.o", object: "qux.o", kind: "const", state: "y", line: 8},
	}
	if !reflect.DeepEqual(gotMembers, wantMembers) {
		t.Fatalf("composite members mismatch\nwant: %#v\n got: %#v", wantMembers, gotMembers)
	}
}

func TestParseKbuildHandlesAssignmentModifiers(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`export objects := exported.o
override objects += override.o
private objects += private.o
obj-y += $(objects)
override define wrapped :=
wrapped.o
endef
obj-y += $(wrapped)
export obj-y += direct.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "exported.o", kind: "const", state: "y", line: 4},
		{object: "override.o", kind: "const", state: "y", line: 4},
		{object: "private.o", kind: "const", state: "y", line: 4},
		{object: "wrapped.o", kind: "const", state: "y", line: 8},
		{object: "direct.o", kind: "const", state: "y", line: 9},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildHandlesVariableDirectives(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`export exported_empty
ifeq ("$(origin exported_empty)","file")
obj-y += export-origin.o
endif
ifeq ("$(flavor exported_empty)","simple")
obj-y += export-flavor.o
endif
export later
later = later.o
unexport later
ifeq ("$(origin later)","file")
obj-y += unexport-keeps-origin.o
endif
obj-y += $(later)
temp := temp.o
undefine temp
ifeq ("$(origin temp)","undefined")
obj-y += undefine-origin.o
endif
again := again.o
override undefine again
ifeq ("$(origin again)","undefined")
obj-y += override-undefine.o
endif
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "export-origin.o", kind: "const", state: "y", line: 3},
		{object: "export-flavor.o", kind: "const", state: "y", line: 6},
		{object: "unexport-keeps-origin.o", kind: "const", state: "y", line: 12},
		{object: "later.o", kind: "const", state: "y", line: 14},
		{object: "undefine-origin.o", kind: "const", state: "y", line: 18},
		{object: "override-undefine.o", kind: "const", state: "y", line: 23},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildInitialVariablesUseMutableOverlay(t *testing.T) {
	initial := map[string]string{
		"objects":                    "initial.o",
		"UBSAN_SANITIZE_inherited.o": "n",
	}
	kb, err := parseKbuild(strings.NewReader(`ifeq ("$(flavor objects)","simple")
obj-y += initial-is-simple.o
endif
obj-y += $(objects)
objects += appended.o
obj-y += $(objects)
objects ?= ignored.o
undefine objects
ifeq ("$(origin objects)","undefined")
obj-y += initial-was-undefined.o
endif
objects ?= reset.o
obj-y += $(objects)
`), "Kbuild", initial, "")
	if err != nil {
		t.Fatalf("parseKbuild() failed: %v", err)
	}

	gotObjects := kbuildObjectSummaries(kb.Objects)
	wantObjects := []kbuildObjectSummary{
		{object: "initial-is-simple.o", kind: "const", state: "y", line: 2},
		{object: "initial.o", kind: "const", state: "y", line: 4},
		{object: "initial.o", kind: "const", state: "y", line: 6},
		{object: "appended.o", kind: "const", state: "y", line: 6},
		{object: "initial-was-undefined.o", kind: "const", state: "y", line: 10},
		{object: "reset.o", kind: "const", state: "y", line: 13},
	}
	if !reflect.DeepEqual(gotObjects, wantObjects) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", wantObjects, gotObjects)
	}

	wantSettings := []kbuildObjectSetting{{
		Name:   "UBSAN_SANITIZE",
		Object: "inherited.o",
		Value:  "n",
	}}
	if !reflect.DeepEqual(kb.objectSettings, wantSettings) {
		t.Fatalf("object settings mismatch\nwant: %#v\n got: %#v", wantSettings, kb.objectSettings)
	}
	wantInitial := map[string]string{
		"objects":                    "initial.o",
		"UBSAN_SANITIZE_inherited.o": "n",
	}
	if !reflect.DeepEqual(initial, wantInitial) {
		t.Fatalf("initial variables mutated\nwant: %#v\n got: %#v", wantInitial, initial)
	}

	second, err := parseKbuild(strings.NewReader("obj-y += $(objects)\n"), "second/Kbuild", initial, "")
	if err != nil {
		t.Fatalf("parseKbuild(second) failed: %v", err)
	}
	wantSecond := []kbuildObjectSummary{{object: "initial.o", kind: "const", state: "y", line: 1}}
	if got := kbuildObjectSummaries(second.Objects); !reflect.DeepEqual(got, wantSecond) {
		t.Fatalf("second parse objects mismatch\nwant: %#v\n got: %#v", wantSecond, got)
	}
}

func TestParseKbuildExpandsMakeVariableIntrospection(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`recursive = raw$(suffix)
simple := simple.o
suffix = .o
ifeq ("$(origin missing)","undefined")
obj-y += missing-origin.o
endif
ifeq ("$(origin recursive)","file")
obj-y += file-origin.o
endif
ifeq ("$(flavor recursive)","recursive")
obj-y += recursive-flavor.o
endif
ifeq ("$(flavor simple)","simple")
obj-y += simple-flavor.o
endif
obj-y += $(value recursive) $(recursive)
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "missing-origin.o", kind: "const", state: "y", line: 5},
		{object: "file-origin.o", kind: "const", state: "y", line: 8},
		{object: "recursive-flavor.o", kind: "const", state: "y", line: 11},
		{object: "simple-flavor.o", kind: "const", state: "y", line: 14},
		{object: "raw.o", kind: "const", state: "y", line: 16},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildPreservesMakeRulesAndRecipes(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`obj := build
src := source
$(obj)/generated.h: $(src)/input.awk FORCE | $(obj) ; $(call filechk,generated)
	$(call if_changed,generated)
$(obj)/%.o: private objtool-enabled = y
$(eval $(obj)/module.o: $(obj)/part1.o $(obj)/part2.o)
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	gotRules := kbuildRuleSummaries(kb.Rules)
	wantRules := []kbuildRuleSummary{
		{
			targets:       "build/generated.h",
			separator:     ":",
			prerequisites: "source/input.awk FORCE",
			orderOnly:     "build",
			recipe:        "$(call filechk,generated)\n$(call if_changed,generated)",
			line:          3,
		},
		{
			targets:       "build/module.o",
			separator:     ":",
			prerequisites: "build/part1.o build/part2.o",
			line:          6,
		},
	}
	if !reflect.DeepEqual(gotRules, wantRules) {
		t.Fatalf("rules mismatch\nwant: %#v\n got: %#v", wantRules, gotRules)
	}

	gotVars := kbuildTargetVariableSummaries(kb.TargetVariables)
	wantVars := []kbuildTargetVariableSummary{
		{
			targets:   "build/%.o",
			variable:  "objtool-enabled",
			operator:  "=",
			value:     "y",
			modifiers: "private",
			line:      5,
		},
	}
	if !reflect.DeepEqual(gotVars, wantVars) {
		t.Fatalf("target variables mismatch\nwant: %#v\n got: %#v", wantVars, gotVars)
	}
}

func TestParseKbuildRejectsUnterminatedDefine(t *testing.T) {
	_, err := ParseKbuild(strings.NewReader(`define missing_end
obj-y += hidden.o
`), "Kbuild")
	if err == nil {
		t.Fatalf("ParseKbuild() succeeded with unterminated define")
	}
}

func TestParseKbuildEvaluatesStaticConditionals(t *testing.T) {
	_, err := ParseKbuild(strings.NewReader(`enabled := y
disabled :=
ifeq ($(enabled),y)
obj-y += enabled.o
endif
ifneq ($(disabled),)
obj-y += disabled.o
else
obj-y += else.o
endif
ifdef enabled
include child
endif
ifndef disabled
always-y += generated.h
endif
else ifdef enabled
obj-y += invalid.o
endif
`), "Kbuild")
	if err == nil {
		t.Fatalf("ParseKbuild() succeeded with unmatched else")
	}

	kb, err := ParseKbuild(strings.NewReader(`enabled := y
disabled :=
ifeq ($(enabled),y)
obj-y += enabled.o
endif
ifneq ($(disabled),)
obj-y += disabled.o
else
obj-y += else.o
endif
ifdef enabled
include child
endif
ifndef disabled
always-y += generated.h
endif
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	gotObjects := kbuildObjectSummaries(kb.Objects)
	wantObjects := []kbuildObjectSummary{
		{object: "enabled.o", kind: "const", state: "y", line: 4},
		{object: "else.o", kind: "const", state: "y", line: 9},
	}
	if !reflect.DeepEqual(gotObjects, wantObjects) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", wantObjects, gotObjects)
	}

	gotIncludes := kbuildIncludeSummaries(kb.Includes)
	wantIncludes := []kbuildIncludeSummary{
		{path: "child", line: 12},
	}
	if !reflect.DeepEqual(gotIncludes, wantIncludes) {
		t.Fatalf("includes mismatch\nwant: %#v\n got: %#v", wantIncludes, gotIncludes)
	}

	gotGenerated := kbuildGeneratedSummaries(kb.Generated)
	wantGenerated := []kbuildGeneratedSummary{
		{kind: "always", target: "generated.h", condKind: "const", state: "y", line: 15},
	}
	if !reflect.DeepEqual(gotGenerated, wantGenerated) {
		t.Fatalf("generated mismatch\nwant: %#v\n got: %#v", wantGenerated, gotGenerated)
	}
}

func TestParseKbuildEvaluatesConfiguredEmptyConfigInMakeFunctions(t *testing.T) {
	kb, err := parseKbuild(strings.NewReader(`obj-y += dwc3.o
dwc3-y := core.o
ifneq ($(filter y,$(CONFIG_USB_DWC3_GADGET) $(CONFIG_USB_DWC3_DUAL_ROLE)),)
	dwc3-y += gadget.o ep0.o
endif
`), "Kbuild", map[string]string{
		"CONFIG_USB_DWC3_GADGET":    "",
		"CONFIG_USB_DWC3_DUAL_ROLE": "",
	}, "")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := compactMetadataBatchForTest(t, mustParseCompactFixture(t), kb, []NamedConfig{{Name: "base"}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}
	base := configByName(metadata, "base")
	if target := objectTarget(metadata, base, "gadget.o"); target != "" {
		t.Fatalf("disabled gadget branch leaked into composite: %q", target)
	}
	if target := objectTarget(metadata, base, "core.o"); target == "" {
		t.Fatalf("unconditional core member missing")
	}
}

func TestParseKbuildCompleteConfigDoesNotLeakConditionalAppend(t *testing.T) {
	for _, condition := range []struct {
		name string
		open string
	}{
		{name: "ifdef", open: "ifdef CONFIG_64BIT"},
		{name: "ifeq", open: "ifeq ($(CONFIG_64BIT),y)"},
		{name: "filtered ifneq", open: "ifneq ($(filter y,$(CONFIG_64BIT)),)"},
	} {
		t.Run(condition.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "Makefile")
			content := "mmu-$(CONFIG_MMU) := memory.o\n" +
				condition.open + "\n" +
				"mmu-$(CONFIG_MMU) += mseal.o\n" +
				"endif\n" +
				"obj-y := $(mmu-y)\n"
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			for _, config := range []struct {
				name      string
				variables map[string]string
				wantMseal bool
			}{
				{name: "missing is unset", variables: map[string]string{"CONFIG_MMU": "y"}},
				{name: "defined empty is unset", variables: map[string]string{
					"CONFIG_64BIT": "",
					"CONFIG_MMU":   "y",
				}},
				{name: "enabled", variables: map[string]string{
					"CONFIG_64BIT": "y",
					"CONFIG_MMU":   "y",
				}, wantMseal: true},
			} {
				t.Run(config.name, func(t *testing.T) {
					kb, err := ParseKbuildFileWithOptions(path, KbuildOptions{
						Variables:               config.variables,
						ConfigVariablesComplete: true,
					})
					if err != nil {
						t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
					}
					want := []kbuildObjectSummary{
						{object: "memory.o", kind: "const", state: "y", line: 5},
					}
					if config.wantMseal {
						want = append(want, kbuildObjectSummary{object: "mseal.o", kind: "const", state: "y", line: 5})
					}
					if got := kbuildObjectSummaries(kb.Objects); !reflect.DeepEqual(got, want) {
						t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
					}
				})
			}
		})
	}
}

func TestParseKbuildEvaluatesElseIfChains(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`selector := second
ifeq ($(selector),first)
obj-y += first.o
else ifeq ($(selector),second)
obj-y += second.o
else ifeq ($(selector),third)
obj-y += third.o
else
obj-y += fallback.o
endif
ifeq ($(CONFIG_UNKNOWN),y)
obj-y += unknown-y.o
else ifeq ($(CONFIG_UNKNOWN),m)
obj-y += unknown-m.o
else
obj-y += unknown-fallback.o
endif
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "second.o", kind: "const", state: "y", line: 5},
		{object: "unknown-y.o", kind: "config_eq", symbol: "CONFIG_UNKNOWN", state: "y", line: 12},
		{object: "unknown-m.o", kind: "all", line: 14},
		{object: "unknown-fallback.o", kind: "not", line: 16},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}

	_, err = ParseKbuild(strings.NewReader(`ifeq (a,b)
else
else ifeq (a,a)
endif
`), "Kbuild")
	if err == nil {
		t.Fatalf("ParseKbuild() succeeded with else conditional after else")
	}
}

func TestParseKbuildPreservesUnknownConfigConditionals(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`ifeq ($(CONFIG_FEATURE),y)
obj-y += feature.o
else
obj-y += fallback.o
endif
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildObjectSummaries(kb.Objects)
	want := []kbuildObjectSummary{
		{object: "feature.o", kind: "config_eq", symbol: "CONFIG_FEATURE", state: "y", line: 2},
		{object: "fallback.o", kind: "config_ne", symbol: "CONFIG_FEATURE", state: "y", line: 4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildExpandsWildcardFunction(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"first.o", "second.o", "generated/one.h"} {
		fullPath := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) failed: %v", path, err)
		}
		if err := os.WriteFile(fullPath, nil, 0o644); err != nil {
			t.Fatalf("WriteFile(%q) failed: %v", path, err)
		}
	}
	kbuildPath := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuildPath, []byte(`objects := $(wildcard *.o)
obj-y += $(objects)
targets += $(wildcard generated/*.h missing/*)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}

	kb, err := ParseKbuildFile(kbuildPath)
	if err != nil {
		t.Fatalf("ParseKbuildFile() failed: %v", err)
	}

	gotObjects := kbuildObjectSummaries(kb.Objects)
	wantObjects := []kbuildObjectSummary{
		{object: "first.o", kind: "const", state: "y", line: 2},
		{object: "second.o", kind: "const", state: "y", line: 2},
	}
	if !reflect.DeepEqual(gotObjects, wantObjects) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", wantObjects, gotObjects)
	}

	gotGenerated := kbuildGeneratedSummaries(kb.Generated)
	wantGenerated := []kbuildGeneratedSummary{
		{kind: "targets", target: "generated/one.h", condKind: "const", state: "y", line: 3},
	}
	if !reflect.DeepEqual(gotGenerated, wantGenerated) {
		t.Fatalf("generated mismatch\nwant: %#v\n got: %#v", wantGenerated, gotGenerated)
	}
}

func TestParseKbuildCommonFlagPatterns(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`ccflags-y += -Wall -Wextra
ccflags-$(CONFIG_NET) += -DCONFIG_NET_SEEN
subdir-ccflags-${CONFIG_USB} += -Iusb
asflags-y += -Wa,--fatal-warnings
subdir-asflags-$(CONFIG_ARM) += -DARM
CFLAGS_net/core.o += -DNET_CORE
AFLAGS_arch/x86/entry.o += -DENTRY
CFLAGS_REMOVE_net/core.o += -Wno-unused
AFLAGS_REMOVE_arch/x86/entry.o += -Wa,--noexecstack
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildFlagSummaries(kb.Flags)
	want := []kbuildFlagSummary{
		{scope: "global", flags: "-Wall -Wextra", kind: "const", state: "y", line: 1},
		{scope: "global", flags: "-DCONFIG_NET_SEEN", kind: "config", symbol: "CONFIG_NET", line: 2},
		{scope: "global", flags: "-Iusb", kind: "config", symbol: "CONFIG_USB", line: 3},
		{scope: "global", flags: "-Wa,--fatal-warnings", kind: "const", state: "y", line: 4},
		{scope: "global", flags: "-DARM", kind: "config", symbol: "CONFIG_ARM", line: 5},
		{scope: "object", object: "net/core.o", flags: "-DNET_CORE", kind: "const", state: "y", line: 6},
		{scope: "object", object: "arch/x86/entry.o", flags: "-DENTRY", kind: "const", state: "y", line: 7},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flags mismatch\nwant: %#v\n got: %#v", want, got)
	}

	gotRemove := kbuildFlagSummaries(kb.RemoveFlags)
	wantRemove := []kbuildFlagSummary{
		{scope: "object", object: "net/core.o", flags: "-Wno-unused", kind: "const", state: "y", line: 8},
		{scope: "object", object: "arch/x86/entry.o", flags: "-Wa,--noexecstack", kind: "const", state: "y", line: 9},
	}
	if !reflect.DeepEqual(gotRemove, wantRemove) {
		t.Fatalf("remove flags mismatch\nwant: %#v\n got: %#v", wantRemove, gotRemove)
	}
}

func TestParseKbuildLocalKbuildFlags(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`KBUILD_CFLAGS += -DLOCAL -fno-stack-protector $(DISABLE_STACKLEAK_PLUGIN)
KBUILD_CFLAGS := $(filter-out $(CC_FLAGS_LTO),$(KBUILD_CFLAGS))
obj-y += main.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	got := kbuildFlagSummaries(kb.Flags)
	want := []kbuildFlagSummary{
		{scope: "global", flags: "-DLOCAL -fno-stack-protector", kind: "const", state: "y", line: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flags mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildLocalKbuildFlagsWithSelfReferenceAdditions(t *testing.T) {
	tmp := t.TempDir()
	kbuild := filepath.Join(tmp, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`KBUILD_CFLAGS := $(subst $(CC_FLAGS_FTRACE),,$(KBUILD_CFLAGS)) -fpie \
	-I$(srctree)/scripts/dtc/libfdt -include $(srctree)/include/linux/hidden.h
KBUILD_CFLAGS := $(filter-out $(CC_FLAGS_SCS), $(KBUILD_CFLAGS))
obj-y += init.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		RootDir: tmp,
		Variables: map[string]string{
			"CC_FLAGS_FTRACE": "",
			"CC_FLAGS_SCS":    "",
			"srctree":         tmp,
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}

	got := kbuildFlagSummaries(kb.Flags)
	want := []kbuildFlagSummary{
		{scope: "global", flags: "-fpie -I" + filepath.ToSlash(tmp) + "/scripts/dtc/libfdt -include " + filepath.ToSlash(tmp) + "/include/linux/hidden.h", kind: "const", state: "y", line: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flags mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildNormalizesWindowsPathVariablesBeforeSplittingFlags(t *testing.T) {
	const sourceRoot = `D:\_bazel\external\+linux_source_repository+linux_6_18_39`
	for _, tc := range []struct {
		name string
		text string
		vars map[string]string
		line int
	}{
		{
			name: "injected",
			text: "KBUILD_CFLAGS += -include $(srctree)/include/linux/hidden.h\nobj-y += init.o\n",
			vars: map[string]string{"srctree": sourceRoot},
			line: 1,
		},
		{
			name: "assigned",
			text: "srctree := " + sourceRoot + "\nKBUILD_CFLAGS += -include $(srctree)/include/linux/hidden.h\nobj-y += init.o\n",
			line: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kb, err := parseKbuild(strings.NewReader(tc.text), "Kbuild", tc.vars, ".")
			if err != nil {
				t.Fatalf("parseKbuild() failed: %v", err)
			}

			got := kbuildFlagSummaries(kb.Flags)
			want := []kbuildFlagSummary{{
				scope: "global",
				flags: "-include D:/_bazel/external/+linux_source_repository+linux_6_18_39/include/linux/hidden.h",
				kind:  "const",
				state: "y",
				line:  tc.line,
			}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("flags mismatch\nwant: %#v\n got: %#v", want, got)
			}
		})
	}
}

func TestParseKbuildSubstPreservesResolvedWordsWithUnresolvedVariables(t *testing.T) {
	tmp := t.TempDir()
	kbuild := filepath.Join(tmp, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`cflags-y := $(KBUILD_CFLAGS)
cflags-y += -I$(srctree)/scripts/dtc/libfdt
KBUILD_CFLAGS := $(subst $(CC_FLAGS_FTRACE),,$(cflags-y)) -Os
obj-y += init.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		RootDir: tmp,
		Variables: map[string]string{
			"srctree": tmp,
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}

	got := kbuildFlagSummaries(kb.Flags)
	want := []kbuildFlagSummary{
		{scope: "global", flags: "-I" + tmp + "/scripts/dtc/libfdt -Os", kind: "const", state: "y", line: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flags mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestParseKbuildGeneratedTargetsAndIncludes(t *testing.T) {
	kb, err := ParseKbuild(strings.NewReader(`include $(srctree)/scripts/Makefile.lib
-include include/config/auto.conf
always-y += bounds.h
always-$(CONFIG_FOO) += generated/foo.h
extra-y += vmlinux.lds
targets += asm-offsets.s $(dynamic-target)
hostprogs-y += fixdep
userprogs-$(CONFIG_USER) += user-helper
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	gotIncludes := kbuildIncludeSummaries(kb.Includes)
	wantIncludes := []kbuildIncludeSummary{
		{path: "$(srctree)/scripts/Makefile.lib", line: 1},
		{path: "include/config/auto.conf", optional: true, line: 2},
	}
	if !reflect.DeepEqual(gotIncludes, wantIncludes) {
		t.Fatalf("includes mismatch\nwant: %#v\n got: %#v", wantIncludes, gotIncludes)
	}

	gotGenerated := kbuildGeneratedSummaries(kb.Generated)
	wantGenerated := []kbuildGeneratedSummary{
		{kind: "always", target: "bounds.h", condKind: "const", state: "y", line: 3},
		{kind: "always", target: "generated/foo.h", condKind: "config", symbol: "CONFIG_FOO", line: 4},
		{kind: "extra", target: "vmlinux.lds", condKind: "const", state: "y", line: 5},
		{kind: "targets", target: "asm-offsets.s", condKind: "const", state: "y", line: 6},
		{kind: "hostprogs", target: "fixdep", condKind: "const", state: "y", line: 7},
		{kind: "userprogs", target: "user-helper", condKind: "config", symbol: "CONFIG_USER", line: 8},
	}
	if !reflect.DeepEqual(gotGenerated, wantGenerated) {
		t.Fatalf("generated mismatch\nwant: %#v\n got: %#v", wantGenerated, gotGenerated)
	}
}

func TestParseKbuildFileTreeFollowsExpandedStaticIncludes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "root"), []byte(`children := $(wildcard child*)
include $(children)
include $(srctree)/child
-include missing
include vars
obj-y += root.o $(from_vars) $(from_makefile_list)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(root) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte(`obj-y += child.o
include grandchild
`), 0o644); err != nil {
		t.Fatalf("WriteFile(child) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child-extra"), []byte(`obj-y += child-extra.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(child-extra) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "grandchild"), []byte(`always-y += generated.h
`), 0o644); err != nil {
		t.Fatalf("WriteFile(grandchild) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vars"), []byte(`from_vars := from-vars.o
from_makefile_list := $(notdir $(lastword $(MAKEFILE_LIST))).o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(vars) failed: %v", err)
	}

	kb, err := ParseKbuildFileTree(filepath.Join(dir, "root"), KbuildOptions{RootDir: dir})
	if err != nil {
		t.Fatalf("ParseKbuildFileTree() failed: %v", err)
	}
	gotObjects := kbuildObjectSummaries(kb.Objects)
	wantObjects := []kbuildObjectSummary{
		{object: "child.o", kind: "const", state: "y", line: 1},
		{object: "child-extra.o", kind: "const", state: "y", line: 1},
		{object: "root.o", kind: "const", state: "y", line: 6},
		{object: "from-vars.o", kind: "const", state: "y", line: 6},
		{object: "vars.o", kind: "const", state: "y", line: 6},
	}
	if !reflect.DeepEqual(gotObjects, wantObjects) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", wantObjects, gotObjects)
	}
	gotGenerated := kbuildGeneratedSummaries(kb.Generated)
	wantGenerated := []kbuildGeneratedSummary{
		{kind: "always", target: "generated.h", condKind: "const", state: "y", line: 1},
	}
	if !reflect.DeepEqual(gotGenerated, wantGenerated) {
		t.Fatalf("generated mismatch\nwant: %#v\n got: %#v", wantGenerated, gotGenerated)
	}
}

func TestParseKbuildDirectoryTreePrefixesObjectsAndConditions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`obj-y += init/
obj-$(CONFIG_NET) += net/
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "init"), 0o755); err != nil {
		t.Fatalf("MkdirAll(init) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "init", "Makefile"), []byte(`ccflags-y += -DINIT
obj-y += main.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(init/Makefile) failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "net"), 0o755); err != nil {
		t.Fatalf("MkdirAll(net) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "net", "Makefile"), []byte(`obj-y += core.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(net/Makefile) failed: %v", err)
	}

	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{RootDir: dir})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}

	gotObjects := kbuildObjectSummaries(kb.Objects)
	wantObjects := []kbuildObjectSummary{
		{object: "init/main.o", kind: "const", state: "y", line: 2},
		{object: "net/core.o", kind: "config", symbol: "CONFIG_NET", line: 1},
	}
	if !reflect.DeepEqual(gotObjects, wantObjects) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", wantObjects, gotObjects)
	}
	gotFlags := kbuildFlagSummaries(kb.Flags)
	wantFlags := []kbuildFlagSummary{
		{scope: "global", directory: "init", flags: "-DINIT", kind: "const", state: "y", line: 1},
	}
	if !reflect.DeepEqual(gotFlags, wantFlags) {
		t.Fatalf("flags mismatch\nwant: %#v\n got: %#v", wantFlags, gotFlags)
	}
}

func TestParseKbuildDirectoryTreeScopesFlagsByTraversalRatherThanPath(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"Kbuild": `KBUILD_AFLAGS += -m64
obj-y += arch/x86/boot/startup/
subdir- += arch/x86/boot
`,
		"arch/x86/boot/Makefile": `KBUILD_AFLAGS := -m16 -D_SETUP
subdir- += compressed
obj-y += setup.o startup/la57toggle.o
`,
		"arch/x86/boot/compressed/Makefile": "obj-y += head.o\n",
		"arch/x86/boot/startup/Makefile": `KBUILD_AFLAGS += -D__DISABLE_EXPORTS
lib-y += la57toggle.o
`,
	}
	for path, content := range files {
		fullPath := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) failed: %v", filepath.Dir(fullPath), err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) failed: %v", path, err)
		}
	}

	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{RootDir: dir})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	objects := kb.resolvedObjects(&ResolvedConfig{})
	flagsFor := func(name string) []string {
		t.Helper()
		object := objects.byName[name]
		if object == nil {
			t.Fatalf("resolved object %q is missing", name)
		}
		var flags []string
		for _, group := range object.flags {
			flags = append(flags, group.values...)
		}
		return flags
	}

	startupFlags := flagsFor("arch/x86/boot/startup/la57toggle.o")
	for _, want := range []string{"-m64", "-D__DISABLE_EXPORTS"} {
		if !slices.Contains(startupFlags, want) {
			t.Errorf("startup flags = %#v, want %q", startupFlags, want)
		}
	}
	for _, unwanted := range []string{"-m16", "-D_SETUP"} {
		if slices.Contains(startupFlags, unwanted) {
			t.Errorf("startup flags = %#v, unexpectedly contain discovery-only %q", startupFlags, unwanted)
		}
	}

	for _, object := range []string{
		"arch/x86/boot/setup.o",
		"arch/x86/boot/compressed/head.o",
	} {
		flags := flagsFor(object)
		for _, want := range []string{"-m64", "-m16", "-D_SETUP"} {
			if !slices.Contains(flags, want) {
				t.Errorf("%s flags = %#v, want %q", object, flags, want)
			}
		}
	}
}

func TestParseKbuildDirectoryTreeFiltersActionTimeRootMakefileKbuildFlags(t *testing.T) {
	tests := []struct {
		name     string
		srcarch  string
		makefile string
		kept     []string
	}{
		{
			name:    "riscv-empty-march",
			srcarch: "riscv",
			makefile: `riscv-march-y :=
KBUILD_CFLAGS += -march=$(riscv-march-y)
KBUILD_CFLAGS += -mno-save-restore -mstrict-align
KBUILD_CPPFLAGS += -I $(srctree)/arch/riscv -DKEEP_RISCV_ROOT
`,
			kept: []string{"-I", "$(srctree)/arch/riscv", "-DKEEP_RISCV_ROOT"},
		},
		{
			name:    "powerpc-empty-canary-offset",
			srcarch: "powerpc",
			makefile: `canary-offset :=
KBUILD_CFLAGS += -mstack-protector-guard=tls
KBUILD_CFLAGS += -mstack-protector-guard-offset=$(canary-offset)
KBUILD_CPPFLAGS += -I $(srctree)/arch/powerpc -DKEEP_POWERPC_ROOT
`,
			kept: []string{"-I", "$(srctree)/arch/powerpc", "-DKEEP_POWERPC_ROOT"},
		},
		{
			name:    "arm64-action-time-defines",
			srcarch: "arm64",
			makefile: `asm-arch := armv8.4-a
KASAN_SHADOW_SCALE_SHIFT := 3
KBUILD_CFLAGS += -DARM64_ASM_ARCH='"$(asm-arch)"'
KBUILD_CFLAGS += -DKASAN_SHADOW_SCALE_SHIFT=$(KASAN_SHADOW_SCALE_SHIFT)
KBUILD_CPPFLAGS += -DKASAN_SHADOW_SCALE_SHIFT=$(KASAN_SHADOW_SCALE_SHIFT)
KBUILD_AFLAGS += -DKASAN_SHADOW_SCALE_SHIFT=$(KASAN_SHADOW_SCALE_SHIFT)
KBUILD_AFLAGS += -include $(srctree)/arch/arm64/include/asm/keep.h
`,
			kept: []string{"-include", "$(srctree)/arch/arm64/include/asm/keep.h"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`KBUILD_CFLAGS += -fprimary
obj-y += child/
`), 0o644); err != nil {
				t.Fatalf("WriteFile(Kbuild) failed: %v", err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "child"), 0o755); err != nil {
				t.Fatalf("MkdirAll(child) failed: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "child", "Makefile"), []byte(`KBUILD_CFLAGS += -flocal
obj-y += local.o
`), 0o644); err != nil {
				t.Fatalf("WriteFile(child/Makefile) failed: %v", err)
			}
			archDir := filepath.Join(dir, "arch", tt.srcarch)
			if err := os.MkdirAll(archDir, 0o755); err != nil {
				t.Fatalf("MkdirAll(arch/%s) failed: %v", tt.srcarch, err)
			}
			archMakefile := filepath.Join(archDir, "Makefile")
			if err := os.WriteFile(archMakefile, []byte(tt.makefile+"ccflags-y += -farch-directory\n"), 0o644); err != nil {
				t.Fatalf("WriteFile(arch/%s/Makefile) failed: %v", tt.srcarch, err)
			}

			kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{
				RootDir:       dir,
				RootMakefiles: []string{archMakefile},
				Variables: map[string]string{
					"ARCH":    tt.srcarch,
					"SRCARCH": tt.srcarch,
				},
			})
			if err != nil {
				t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
			}

			got := map[string]string{}
			for _, flag := range kb.Flags {
				for _, value := range flag.Flags {
					got[value] = flag.Directory
				}
			}
			want := map[string]string{
				"-fprimary":        "",
				"-farch-directory": "",
				"-flocal":          "child",
			}
			for _, value := range tt.kept {
				value = strings.ReplaceAll(value, "$(srctree)", filepath.ToSlash(dir))
				want[value] = ""
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("flags mismatch\nwant: %#v\n got: %#v", want, got)
			}
		})
	}
}

func TestParseKbuildDirectoryTreePropagatesExportedRootVariables(t *testing.T) {
	dir := t.TempDir()
	for _, child := range []string{"kernel", "mm"} {
		if err := os.MkdirAll(filepath.Join(dir, "arch", "arm", child), 0o755); err != nil {
			t.Fatalf("MkdirAll(arch/arm/%s) failed: %v", child, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte("obj-y += arch/arm/kernel/ arch/arm/mm/\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "arch", "arm", "Makefile"), []byte(`MMUEXT := -nommu
ifeq ($(CONFIG_MMU),y)
MMUEXT :=
endif
TEXT_OFFSET := 0x00008000
export TEXT_OFFSET MMUEXT
`), 0o644); err != nil {
		t.Fatalf("WriteFile(arch/arm/Makefile) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "arch", "arm", "kernel", "Makefile"), []byte(`obj-y += head$(MMUEXT).o
AFLAGS_head$(MMUEXT).o := -DTEXT_OFFSET=$(TEXT_OFFSET)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(arch/arm/kernel/Makefile) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "arch", "arm", "mm", "Makefile"), []byte("obj-y += dma-mapping$(MMUEXT).o\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(arch/arm/mm/Makefile) failed: %v", err)
	}

	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{
		RootDir:                 dir,
		RootMakefiles:           []string{"arch/arm/Makefile"},
		Variables:               map[string]string{"CONFIG_MMU": "y"},
		ConfigVariablesComplete: true,
	})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}

	gotObjects := kbuildObjectSummaries(kb.Objects)
	wantObjects := []kbuildObjectSummary{
		{object: "arch/arm/kernel/head.o", kind: "const", state: "y", line: 1},
		{object: "arch/arm/mm/dma-mapping.o", kind: "const", state: "y", line: 1},
	}
	if !reflect.DeepEqual(gotObjects, wantObjects) {
		t.Fatalf("objects mismatch\nwant: %#v\n got: %#v", wantObjects, gotObjects)
	}
	gotFlags := kbuildFlagSummaries(kb.Flags)
	wantFlags := []kbuildFlagSummary{{
		scope:  "object",
		object: "arch/arm/kernel/head.o",
		flags:  "-DTEXT_OFFSET=0x00008000",
		kind:   "const",
		state:  "y",
		line:   2,
	}}
	if !reflect.DeepEqual(gotFlags, wantFlags) {
		t.Fatalf("flags mismatch\nwant: %#v\n got: %#v", wantFlags, gotFlags)
	}
}

func TestCompactMetadataResolvesCompositeMembers(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader(`obj-$(CONFIG_NET) += net/stack.o
net/stack-y += net/base.o
net/stack-$(CONFIG_DEBUG) += net/debug.o
net/stack-${CONFIG_NET} += net/selected.o
CFLAGS_net/base.o += -DBASE
AFLAGS_net/debug.o += -DDEBUG_ASM
hostprogs-y += host-helper.o
always-y += generated.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{
		{Name: "base", Flags: map[string]string{"CONFIG_NET": "y"}},
		{Name: "debug", Flags: map[string]string{"CONFIG_NET": "y", "CONFIG_DEBUG": "y"}},
		{Name: "off", Flags: map[string]string{"CONFIG_DEBUG": "y"}},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	base := configByName(metadata, "base")
	if target := objectTarget(metadata, base, "net/stack.o"); target != "" {
		t.Fatalf("base config kept built-in composite parent in image targets: %q", target)
	}
	if target := objectTarget(metadata, base, "net/base.o"); target == "" {
		t.Fatalf("base config does not include unconditional composite member: %#v", metadata.Configs)
	} else if variant := variantByTarget(metadata, target); strings.Join(variant.Flags, " ") != "-DBASE" {
		t.Fatalf("net/base.o flags = %#v, want -DBASE", variant.Flags)
	}
	if objectTarget(metadata, base, "net/debug.o") != "" {
		t.Fatalf("base config unexpectedly includes conditional debug member")
	}
	if objectTarget(metadata, base, "host-helper.o") != "" || objectTarget(metadata, base, "generated.o") != "" {
		t.Fatalf("non-object helper/generated lists leaked into object metadata")
	}

	debug := configByName(metadata, "debug")
	if target := objectTarget(metadata, debug, "net/debug.o"); target == "" {
		t.Fatalf("debug config does not include conditional composite member")
	} else if variant := variantByTarget(metadata, target); strings.Join(variant.Flags, " ") != "-DDEBUG_ASM" {
		t.Fatalf("net/debug.o flags = %#v, want -DDEBUG_ASM", variant.Flags)
	}
	if target := objectTarget(metadata, debug, "net/selected.o"); target == "" {
		t.Fatalf("debug config does not include ${CONFIG_NET} composite member")
	} else if variant := variantByTarget(metadata, target); variant.configFragment["CONFIG_NET"] != "y" {
		t.Fatalf("net/selected.o CONFIG_NET footprint = %q, want y", variant.configFragment["CONFIG_NET"])
	}

	off := configByName(metadata, "off")
	if objectTarget(metadata, off, "net/stack.o") != "" || objectTarget(metadata, off, "net/base.o") != "" || objectTarget(metadata, off, "net/debug.o") != "" {
		t.Fatalf("disabled parent object leaked composite members into off config")
	}
}

func TestCompactMetadataCompositeModuleMembersFollowParentMode(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "Composite modules"

config MODULES
	bool "Enable modules"
	modules

config STACK
	tristate "Stack"

config FEATURE
	tristate "Feature"
`)
	kb, err := ParseKbuild(strings.NewReader(`obj-$(CONFIG_STACK) += net/stack.o
net/stack-y += net/base.o
net/stack-$(CONFIG_FEATURE) += net/debug.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{
		{Name: "module", Flags: map[string]string{"CONFIG_MODULES": "y", "CONFIG_STACK": "m", "CONFIG_FEATURE": "m"}},
		{Name: "builtin", Flags: map[string]string{"CONFIG_MODULES": "y", "CONFIG_STACK": "y", "CONFIG_FEATURE": "m"}},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	moduleConfig := configByName(metadata, "module")
	if target := objectTarget(metadata, moduleConfig, "net/stack.o"); target != "" {
		t.Fatalf("module composite parent %q leaked into image targets", target)
	}
	moduleTarget := moduleObjectTarget(metadata, moduleConfig, "net/stack.o")
	if moduleTarget == "" {
		t.Fatalf("module config does not include m composite parent: %#v", moduleConfig)
	}
	moduleParent := variantByTarget(metadata, moduleTarget)
	if len(moduleParent.Members) == 0 {
		t.Fatalf("module composite parent has no members: %#v", moduleParent)
	}
	moduleDebugTarget := ""
	for _, target := range moduleParent.Members {
		if variantByTarget(metadata, target).Object == "net/debug.o" {
			moduleDebugTarget = target
			break
		}
	}
	if moduleDebugTarget == "" {
		t.Fatalf("module config does not include m composite member")
	}
	if variant := variantByTarget(metadata, moduleDebugTarget); variant.Mode != "m" {
		t.Fatalf("module debug member mode = %q, want m", variant.Mode)
	}
	if objectTarget(metadata, configByName(metadata, "builtin"), "net/debug.o") != "" {
		t.Fatalf("builtin parent unexpectedly includes m-only composite member")
	}
}

type kbuildObjectSummary struct {
	object string
	kind   string
	symbol string
	state  string
	line   int
}

func kbuildObjectSummaries(objects []KbuildObject) []kbuildObjectSummary {
	out := make([]kbuildObjectSummary, 0, len(objects))
	for _, object := range objects {
		out = append(out, kbuildObjectSummary{
			object: object.Object,
			kind:   object.Condition.Kind,
			symbol: object.Condition.Symbol,
			state:  object.Condition.State,
			line:   object.Position.Line,
		})
	}
	return out
}

type kbuildCompositeMemberSummary struct {
	composite string
	object    string
	kind      string
	symbol    string
	state     string
	line      int
}

func kbuildCompositeMemberSummaries(members []kbuildCompositeMember) []kbuildCompositeMemberSummary {
	out := make([]kbuildCompositeMemberSummary, 0, len(members))
	for _, member := range members {
		out = append(out, kbuildCompositeMemberSummary{
			composite: member.Composite,
			object:    member.Object,
			kind:      member.Condition.Kind,
			symbol:    member.Condition.Symbol,
			state:     member.Condition.State,
			line:      member.Position.Line,
		})
	}
	return out
}

type kbuildFlagSummary struct {
	scope     string
	object    string
	directory string
	flags     string
	kind      string
	symbol    string
	state     string
	line      int
}

func kbuildFlagSummaries(flags []KbuildFlag) []kbuildFlagSummary {
	out := make([]kbuildFlagSummary, 0, len(flags))
	for _, flag := range flags {
		out = append(out, kbuildFlagSummary{
			scope:     flag.Scope,
			object:    flag.Object,
			directory: flag.Directory,
			flags:     strings.Join(flag.Flags, " "),
			kind:      flag.Condition.Kind,
			symbol:    flag.Condition.Symbol,
			state:     flag.Condition.State,
			line:      flag.Position.Line,
		})
	}
	return out
}

type kbuildDirSummary struct {
	kind      string
	directory string
	condKind  string
	symbol    string
	state     string
	line      int
}

func kbuildDirSummaries(dirs []KbuildDir) []kbuildDirSummary {
	out := make([]kbuildDirSummary, 0, len(dirs))
	for _, dir := range dirs {
		out = append(out, kbuildDirSummary{
			kind:      dir.Kind,
			directory: dir.Directory,
			condKind:  dir.Condition.Kind,
			symbol:    dir.Condition.Symbol,
			state:     dir.Condition.State,
			line:      dir.Position.Line,
		})
	}
	return out
}

type kbuildGeneratedSummary struct {
	kind     string
	target   string
	condKind string
	symbol   string
	state    string
	line     int
}

func kbuildGeneratedSummaries(targets []KbuildTarget) []kbuildGeneratedSummary {
	out := make([]kbuildGeneratedSummary, 0, len(targets))
	for _, target := range targets {
		out = append(out, kbuildGeneratedSummary{
			kind:     target.Kind,
			target:   target.Target,
			condKind: target.Condition.Kind,
			symbol:   target.Condition.Symbol,
			state:    target.Condition.State,
			line:     target.Position.Line,
		})
	}
	return out
}

type kbuildIncludeSummary struct {
	path     string
	optional bool
	line     int
}

func kbuildIncludeSummaries(includes []KbuildInclude) []kbuildIncludeSummary {
	out := make([]kbuildIncludeSummary, 0, len(includes))
	for _, include := range includes {
		out = append(out, kbuildIncludeSummary{
			path:     include.Path,
			optional: include.Optional,
			line:     include.Position.Line,
		})
	}
	return out
}

type kbuildRuleSummary struct {
	targets       string
	separator     string
	prerequisites string
	orderOnly     string
	recipe        string
	line          int
}

func kbuildRuleSummaries(rules []KbuildRule) []kbuildRuleSummary {
	out := make([]kbuildRuleSummary, 0, len(rules))
	for _, rule := range rules {
		out = append(out, kbuildRuleSummary{
			targets:       strings.Join(rule.Targets, " "),
			separator:     rule.Separator,
			prerequisites: strings.Join(rule.Prerequisites, " "),
			orderOnly:     strings.Join(rule.OrderOnly, " "),
			recipe:        strings.Join(rule.Recipe, "\n"),
			line:          rule.Position.Line,
		})
	}
	return out
}

type kbuildTargetVariableSummary struct {
	targets   string
	variable  string
	operator  string
	value     string
	modifiers string
	line      int
}

func kbuildTargetVariableSummaries(variables []KbuildTargetVariable) []kbuildTargetVariableSummary {
	out := make([]kbuildTargetVariableSummary, 0, len(variables))
	for _, variable := range variables {
		out = append(out, kbuildTargetVariableSummary{
			targets:   strings.Join(variable.Targets, " "),
			variable:  variable.Variable,
			operator:  variable.Operator,
			value:     variable.Value,
			modifiers: strings.Join(variable.Modifiers, " "),
			line:      variable.Position.Line,
		})
	}
	return out
}
