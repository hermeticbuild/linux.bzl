package kconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/ccprofile"
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

func TestKbuildStructuralProbesUseExactCCProfileRecords(t *testing.T) {
	ccPrefix := []string{"-Werror", "-DCTX=1", "-mcmodel=kernel"}
	asPrefix := []string{"-Werror", "-DCTX=1", "-DASSEMBLY"}
	ldPrefix := []string{"-m", "elf_x86_64"}
	profile := testCCProfile(
		ccprofile.StructuralProbe{
			Kind:       "cc-option",
			Language:   "c",
			PrefixArgv: ccPrefix,
			Argv:       []string{"-fgood"},
			Supported:  true,
		},
		ccprofile.StructuralProbe{
			Kind:       "cc-option",
			Language:   "c",
			PrefixArgv: ccPrefix,
			Argv:       []string{"-fbad"},
			Supported:  false,
		},
		ccprofile.StructuralProbe{
			Kind:       "cc-option-yn",
			Language:   "c",
			PrefixArgv: ccPrefix,
			Argv:       []string{"-fyes"},
			Supported:  true,
		},
		ccprofile.StructuralProbe{
			Kind:       "cc-disable-warning",
			Language:   "c",
			PrefixArgv: ccPrefix,
			Argv:       []string{"unused"},
			Supported:  true,
		},
		ccprofile.StructuralProbe{
			Kind:       "as-option",
			Language:   "asm",
			PrefixArgv: asPrefix,
			Argv:       []string{"-masm-good"},
			Supported:  true,
		},
		ccprofile.StructuralProbe{
			Kind:       "ld-option",
			Language:   "link",
			PrefixArgv: ldPrefix,
			Argv:       []string{"--ld-good"},
			Supported:  true,
		},
	)

	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`KBUILD_CPPFLAGS := -DCTX=1
KBUILD_CFLAGS := -mcmodel=kernel
KBUILD_AFLAGS := -DASSEMBLY
KBUILD_LDFLAGS := -m elf_x86_64
obj-y += core.o
ccflags-y += $(call cc-option,-fgood) $(call cc-option,-fbad,-ffallback)
ccflags-y += $(call cc-disable-warning, unused)
ccflags-y += $(call cc-option-yn,-fyes)
asflags-y += $(call as-option,-masm-good)
ccflags-y += $(call ld-option,--ld-good)
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}
	recorder := NewStructuralProbeRecorder()
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		CCProfile:     profile,
		ProbeRecorder: recorder,
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}

	flagsByLine := map[int][]string{}
	for _, flags := range kb.Flags {
		flagsByLine[flags.Position.Line] = append(
			flagsByLine[flags.Position.Line],
			flags.Flags...,
		)
	}
	for line, want := range map[int][]string{
		6:  {"-fgood", "-ffallback"},
		7:  {"-Wno-unused"},
		8:  {"y"},
		9:  {"-masm-good"},
		10: {"--ld-good"},
	} {
		if got := flagsByLine[line]; !reflect.DeepEqual(got, want) {
			t.Errorf("line %d flags = %v, want %v", line, got, want)
		}
	}
	if got, want := len(recorder.Requests()), len(profile.StructuralProbes); got != want {
		t.Fatalf("profile-backed request count = %d, want %d", got, want)
	}
}

func TestKbuildStructuralProbeFailsClosedOnMissingPrefix(t *testing.T) {
	profile := testCCProfile(ccprofile.StructuralProbe{
		Kind:       "cc-option",
		Language:   "c",
		PrefixArgv: []string{"-Werror", "-DOTHER=1"},
		Argv:       []string{"-fgood"},
		Supported:  true,
	})
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`KBUILD_CPPFLAGS := -DCTX=1
obj-y += core.o
ccflags-y += $(call cc-option,-fgood)
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}

	_, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		CCProfile: profile,
	})
	if err == nil {
		t.Fatal("ParseKbuildFileWithOptions() succeeded without an exact structural probe")
	}
	for _, want := range []string{
		"missing CC profile structural probe",
		`kind="cc-option"`,
		`language="c"`,
		`prefix_argv=["-Werror" "-DCTX=1"]`,
		`argv=["-fgood"]`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestKbuildStructuralProbeRecorderBootstrapsMissingProfileRecords(t *testing.T) {
	profile := testCCProfile()
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`ccflags-y += $(call cc-option,-fno-conserve-stack,-ffallback)
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}
	recorder := NewStructuralProbeRecorder()
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		CCProfile:     profile,
		ProbeRecorder: recorder,
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	if got, want := kb.Flags[0].Flags, []string{"-ffallback"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy bootstrap fallback = %v, want %v", got, want)
	}
	requests := recorder.Requests()
	if got, want := len(requests), 1; got != want {
		t.Fatalf("missing request count = %d, want %d: %#v", got, want, requests)
	}
	if got, want := requests[0].Argv, []string{"-fno-conserve-stack"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("missing request argv = %v, want %v", got, want)
	}
}

func TestKbuildStructuralProbeUsesCCOptionCFLAGSWhenDefined(t *testing.T) {
	profile := testCCProfile(ccprofile.StructuralProbe{
		Kind:       "cc-option",
		Language:   "c",
		PrefixArgv: []string{"-Werror", "-DPROFILE_CONTEXT=1"},
		Argv:       []string{"-fgood"},
		Supported:  true,
	})
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`KBUILD_CFLAGS := -DLEGACY_CONTEXT=1
CC_OPTION_CFLAGS := -DPROFILE_CONTEXT=1
obj-y += core.o
ccflags-y += $(call cc-option,-fgood)
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}

	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		CCProfile: profile,
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	var got []string
	for _, flags := range kb.Flags {
		if flags.Position.Line == 4 {
			got = append(got, flags.Flags...)
		}
	}
	if want := []string{"-fgood"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CC_OPTION_CFLAGS probe result = %v, want %v", got, want)
	}
}

func TestKbuildStructuralProbeRecorderCapturesCanonicalRequests(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`KBUILD_CPPFLAGS := -DCTX=1
KBUILD_CFLAGS := -mcmodel=kernel
KBUILD_AFLAGS := -DASSEMBLY
KBUILD_LDFLAGS := -m elf_x86_64
obj-y += core.o
ccflags-y += $(call cc-option,-fgood) $(call cc-option,-fno-conserve-stack,-ffallback)
ccflags-y += $(call cc-disable-warning, unused)
ccflags-y += $(call cc-option-yn,-mrecord-mcount)
asflags-y += $(call as-option,-masm-good)
ccflags-y += $(call ld-option,--ld-good)
ccflags-y += $(call cc-option,-fgood)
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}
	recorder := NewStructuralProbeRecorder()
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		ProbeRecorder: recorder,
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}

	flagsByLine := map[int][]string{}
	for _, flags := range kb.Flags {
		flagsByLine[flags.Position.Line] = append(
			flagsByLine[flags.Position.Line],
			flags.Flags...,
		)
	}
	if got, want := flagsByLine[6], []string{"-fgood", "-ffallback"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy cc-option fallback flags = %v, want %v", got, want)
	}
	if got, want := flagsByLine[8], []string{"n"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy cc-option-yn result = %v, want %v", got, want)
	}

	requests := recorder.Requests()
	if got, want := len(requests), 6; got != want {
		t.Fatalf("request count = %d, want %d: %#v", got, want, requests)
	}
	for index, request := range requests {
		if index != 0 && requests[index-1].ID >= request.ID {
			t.Fatalf("requests are not canonically ordered: %#v", requests)
		}
		probe := ccprofile.StructuralProbe{
			Kind:       request.Kind,
			Language:   request.Language,
			PrefixArgv: request.PrefixArgv,
			Argv:       request.Argv,
			Supported:  true,
		}
		if got, want := request.ID, ccprofile.StructuralProbeID(probe); got != want {
			t.Errorf("request ID = %s, want %s for %#v", got, want, request)
		}
	}

	firstJSON, err := recorder.JSON()
	if err != nil {
		t.Fatalf("JSON() failed: %v", err)
	}
	secondJSON, err := recorder.JSON()
	if err != nil {
		t.Fatalf("second JSON() failed: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("recorder JSON is nondeterministic:\n%s\n%s", firstJSON, secondJSON)
	}
	if strings.Contains(string(firstJSON), `"supported"`) {
		t.Fatalf("bootstrap requests contain an unmeasured supported result:\n%s", firstJSON)
	}
	var manifest StructuralProbeRequestManifest
	if err := json.Unmarshal(firstJSON, &manifest); err != nil {
		t.Fatalf("Unmarshal() failed: %v", err)
	}
	if got, want := manifest.Schema, StructuralProbeRequestsSchema; got != want {
		t.Fatalf("manifest schema = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(manifest.StructuralProbes, requests) {
		t.Fatalf("manifest requests = %#v, want %#v", manifest.StructuralProbes, requests)
	}
}

func TestKbuildStructuralProbeRecorderDefersRecursiveMacroCalls(t *testing.T) {
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`tune = $(call cc-option,-mtune=$(1),$(2))
ccflags-y += $(call tune,generic,fallback)
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}

	recorder := NewStructuralProbeRecorder()
	if _, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		ProbeRecorder: recorder,
	}); err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	requests := recorder.Requests()
	if got, want := len(requests), 1; got != want {
		t.Fatalf("request count = %d, want %d: %#v", got, want, requests)
	}
	if got, want := requests[0].Argv, []string{"-mtune=generic"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("request argv = %v, want %v", got, want)
	}
}

func TestKbuildStructuralProbeRecorderCanonicalizesMakeContext(t *testing.T) {
	parse := func(dir string) []StructuralProbeRequest {
		t.Helper()
		kbuild := filepath.Join(dir, "Kbuild")
		if err := os.WriteFile(kbuild, []byte(`KBUILD_CFLAGS := $(KBUILD_CFLAGS) -DLOCAL $(unknown) \
	$(call try-run,echo int main(void) { return 0; },-DPROBED)
KBUILD_CPPFLAGS += -I$(srctree)/include -I$(objtree)/include
ccflags-y += $(call cc-option,-Wa$(comma)-mrelax-relocations=no)
`), 0o644); err != nil {
			t.Fatalf("WriteFile() failed: %v", err)
		}
		recorder := NewStructuralProbeRecorder()
		if _, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
			RootDir: dir,
			Variables: map[string]string{
				"objtree": filepath.Join(dir, "out"),
				"srctree": dir,
			},
			ProbeRecorder: recorder,
		}); err != nil {
			t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
		}
		return recorder.Requests()
	}

	left := parse(t.TempDir())
	right := parse(t.TempDir())
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("checkout-dependent requests:\nleft:  %#v\nright: %#v", left, right)
	}
	if got, want := len(left), 1; got != want {
		t.Fatalf("request count = %d, want %d: %#v", got, want, left)
	}
	if got, want := left[0].PrefixArgv, []string{
		"-Werror",
		"-I" + StructuralProbeSourceRoot + "/include",
		"-I" + StructuralProbeObjectRoot + "/include",
		"-DLOCAL",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical prefix argv = %v, want %v", got, want)
	}
	if got, want := left[0].Argv, []string{"-Wa,-mrelax-relocations=no"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical argv = %v, want %v", got, want)
	}
	for _, arg := range append(append([]string{}, left[0].PrefixArgv...), left[0].Argv...) {
		if containsMakeReference(arg) {
			t.Fatalf("canonical request contains unresolved Make syntax: %q", arg)
		}
	}
}

func TestStructuralProbeRecorderJSONUsesEmptyArrays(t *testing.T) {
	recorder := NewStructuralProbeRecorder()
	if err := recorder.Record(ccprofile.StructuralProbe{
		Kind:     "ld-option",
		Language: "link",
		Argv:     []string{"--build-id"},
	}); err != nil {
		t.Fatalf("Record() failed: %v", err)
	}
	data, err := recorder.JSON()
	if err != nil {
		t.Fatalf("JSON() failed: %v", err)
	}
	if strings.Contains(string(data), "null") {
		t.Fatalf("canonical request JSON contains null arrays:\n%s", data)
	}
	if !strings.Contains(string(data), `"prefix_argv": []`) {
		t.Fatalf("canonical request JSON omits an empty prefix array:\n%s", data)
	}
}

func TestKbuildDirectoryTreeSharesStructuralProbeRecorder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`obj-y += root.o child/
ccflags-y += $(call cc-option,-fgood)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "child"), 0o755); err != nil {
		t.Fatalf("Mkdir(child) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child", "Makefile"), []byte(`obj-y += child.o
ccflags-y += $(call cc-option,-fgood)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(child/Makefile) failed: %v", err)
	}

	recorder := NewStructuralProbeRecorder()
	if _, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{
		RootDir:       dir,
		ProbeRecorder: recorder,
	}); err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	requests := recorder.Requests()
	if got, want := len(requests), 1; got != want {
		t.Fatalf("shared recursive request count = %d, want %d: %#v", got, want, requests)
	}
	if got, want := requests[0].Argv, []string{"-fgood"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recursive request argv = %v, want %v", got, want)
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
			"CC_FLAGS_FTRACE": "",
			"srctree":         tmp,
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
