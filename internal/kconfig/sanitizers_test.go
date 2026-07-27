package kconfig

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestKasanKbuildFlagsHonorDirectoryAndObjectSettings(t *testing.T) {
	config := resolvedConfigValues(map[string]string{
		"CONFIG_CC_HAS_KASAN_MEMINTRINSIC_PREFIX": "y",
		"CONFIG_KASAN":               "y",
		"CONFIG_KASAN_GENERIC":       "y",
		"CONFIG_KASAN_INLINE":        "y",
		"CONFIG_KASAN_SHADOW_OFFSET": "0xdffffc0000000000",
		"CONFIG_KASAN_STACK":         "y",
	})
	settings := []kbuildObjectSetting{
		{Name: "KASAN_SANITIZE", Directory: "mm", Value: "n"},
		{Name: "KASAN_SANITIZE", Directory: "mm", Object: "mm/forced.o", Value: "y"},
	}
	want := []string{
		"-fsanitize=kernel-address",
		"-mllvm",
		"-asan-mapping-offset=0xdffffc0000000000",
		"-mllvm",
		"-asan-instrumentation-with-call-threshold=10000",
		"-mllvm",
		"-asan-stack=1",
		"-mllvm",
		"-asan-instrument-allocas=1",
		"-mllvm",
		"-asan-globals=1",
		"-mllvm",
		"-asan-kernel-mem-intrinsic-prefix=1",
	}
	for _, object := range []*resolvedKbuildObject{
		{object: "init/main.o", directory: "init", mode: "y"},
		{object: "drivers/test.o", directory: "drivers", mode: "m"},
		{object: "mm/forced.o", directory: "mm", mode: "y"},
	} {
		if got := kasanKbuildFlags(config, settings, object); !reflect.DeepEqual(got, want) {
			t.Fatalf("kasanKbuildFlags(%s, mode=%s) mismatch\nwant: %#v\n got: %#v", object.object, object.mode, want, got)
		}
	}
	if got := kasanKbuildFlags(config, settings, &resolvedKbuildObject{
		object:    "mm/slab.o",
		directory: "mm",
		mode:      "y",
	}); len(got) != 0 {
		t.Fatalf("directory KASAN opt-out flags = %#v, want none", got)
	}
}

func TestKasanKbuildFlagsModelSoftwareTagsAndNoSanitize(t *testing.T) {
	config := resolvedConfigValues(map[string]string{
		"CONFIG_CC_HAS_KASAN_MEMINTRINSIC_PREFIX": "y",
		"CONFIG_KASAN":               "y",
		"CONFIG_KASAN_SHADOW_OFFSET": "0xdfff800000000000",
		"CONFIG_KASAN_SW_TAGS":       "y",
	})
	object := &resolvedKbuildObject{object: "kernel/work.o", directory: "kernel", mode: "y"}
	want := []string{
		"-fsanitize=kernel-hwaddress",
		"-mllvm",
		"-hwasan-instrument-with-calls=1",
		"-mllvm",
		"-hwasan-instrument-stack=0",
		"-mllvm",
		"-hwasan-use-short-granules=0",
		"-mllvm",
		"-hwasan-inline-all-checks=0",
		"-mllvm",
		"-hwasan-kernel-mem-intrinsic-prefix=1",
	}
	if got := kasanKbuildFlags(config, nil, object); !reflect.DeepEqual(got, want) {
		t.Fatalf("software-tag KASAN flags mismatch\nwant: %#v\n got: %#v", want, got)
	}

	delete(config.Effective, "CONFIG_CC_HAS_KASAN_MEMINTRINSIC_PREFIX")
	settings := []kbuildObjectSetting{{
		Name:      "KASAN_SANITIZE",
		Object:    object.object,
		Directory: object.directory,
		Value:     "n",
	}}
	if got := kasanKbuildFlags(config, settings, object); !reflect.DeepEqual(got, []string{"-fno-builtin"}) {
		t.Fatalf("uninstrumented KASAN flags = %#v, want -fno-builtin", got)
	}
}

func TestKasanKbuildFlagsDoNotEmitEmptyShadowOffset(t *testing.T) {
	config := resolvedConfigValues(map[string]string{
		"CONFIG_KASAN":         "y",
		"CONFIG_KASAN_GENERIC": "y",
	})
	flags := kasanKbuildFlags(config, nil, &resolvedKbuildObject{object: "main.o", mode: "y"})
	for _, flag := range flags {
		if strings.HasPrefix(flag, "-asan-mapping-offset=") {
			t.Fatalf("KASAN flags contain malformed empty mapping offset: %#v", flags)
		}
	}
}

func TestKasanKbuildFlagsGenericMemintrinsicPrefixIsCompilerProbed(t *testing.T) {
	config := resolvedConfigValues(map[string]string{
		"CONFIG_KASAN":               "y",
		"CONFIG_KASAN_GENERIC":       "y",
		"CONFIG_KASAN_SHADOW_OFFSET": "0xdffffc0000000000",
	})
	flags := kasanKbuildFlags(config, nil, &resolvedKbuildObject{object: "main.o", mode: "y"})
	if !slices.Contains(flags, "-asan-kernel-mem-intrinsic-prefix=1") {
		t.Fatalf("generic KASAN flags do not include compiler-probed memintrinsic prefix: %#v", flags)
	}
}

func TestKasanKbuildFlagsFirstSelectorWins(t *testing.T) {
	config := resolvedConfigValues(map[string]string{
		"CONFIG_CC_HAS_KASAN_MEMINTRINSIC_PREFIX": "y",
		"CONFIG_KASAN":         "y",
		"CONFIG_KASAN_GENERIC": "y",
	})
	object := &resolvedKbuildObject{object: "mm/test.o", directory: "mm", mode: "y"}
	for _, test := range []struct {
		name       string
		object     string
		directory  string
		instrument bool
	}{
		{name: "object n overrides directory y", object: "n", directory: "y"},
		{name: "object y overrides directory n", object: "y", directory: "n", instrument: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := []kbuildObjectSetting{
				{Name: "KASAN_SANITIZE", Object: object.object, Directory: object.directory, Value: test.object},
				{Name: "KASAN_SANITIZE", Directory: object.directory, Value: test.directory},
			}
			got := slices.Contains(kasanKbuildFlags(config, settings, object), "-fsanitize=kernel-address")
			if got != test.instrument {
				t.Fatalf("instrumented = %t, want %t", got, test.instrument)
			}
		})
	}
}

func TestKasanAndKcsanExplicitCflagsOverrideDirectoryOptOut(t *testing.T) {
	object := &resolvedKbuildObject{object: "kernel/test.o", directory: "kernel", mode: "y"}
	settings := []kbuildObjectSetting{
		{Name: "KASAN_SANITIZE", Directory: object.directory, Value: "n"},
		{Name: "CFLAGS_KASAN", Object: object.object, Directory: object.directory, Value: "y"},
		{Name: "KCSAN_SANITIZE", Directory: object.directory, Value: "n"},
		{Name: "CFLAGS_KCSAN", Object: object.object, Directory: object.directory, Value: "y"},
	}
	kasan := resolvedConfigValues(map[string]string{
		"CONFIG_KASAN":         "y",
		"CONFIG_KASAN_GENERIC": "y",
	})
	if flags := kasanKbuildFlags(kasan, settings, object); !slices.Contains(flags, "-fsanitize=kernel-address") {
		t.Fatalf("explicit CFLAGS_KASAN did not force instrumentation: %#v", flags)
	}
	kcsan := resolvedConfigValues(map[string]string{"CONFIG_KCSAN": "y"})
	if flags := kcsanKbuildFlags(kcsan, settings, object); !slices.Contains(flags, "-fsanitize=thread") {
		t.Fatalf("explicit CFLAGS_KCSAN did not force instrumentation: %#v", flags)
	}
}

func TestKcsanKbuildFlagsHonorSanitizeAndBarrierSettings(t *testing.T) {
	config := resolvedConfigValues(map[string]string{
		"CONFIG_CC_HAS_TSAN_COMPOUND_READ_BEFORE_WRITE": "y",
		"CONFIG_KCSAN": "y",
	})
	settings := []kbuildObjectSetting{
		{Name: "KCSAN_SANITIZE", Directory: "mm", Value: "n"},
		{Name: "KCSAN_INSTRUMENT_BARRIERS", Directory: "mm", Value: "y"},
		{Name: "KCSAN_SANITIZE", Directory: "mm", Object: "mm/forced.o", Value: "y"},
	}
	wantInstrumented := []string{
		"-fsanitize=thread",
		"-fno-optimize-sibling-calls",
		"-mllvm",
		"-tsan-compound-read-before-write=1",
		"-mllvm",
		"-tsan-distinguish-volatile=1",
		"-mllvm",
		"-tsan-instrument-func-entry-exit=0",
	}
	module := &resolvedKbuildObject{object: "drivers/test.o", directory: "drivers", mode: "m"}
	if got := kcsanKbuildFlags(config, settings, module); !reflect.DeepEqual(got, wantInstrumented) {
		t.Fatalf("module KCSAN flags mismatch\nwant: %#v\n got: %#v", wantInstrumented, got)
	}
	if got := kcsanKbuildFlags(config, settings, &resolvedKbuildObject{
		object:    "mm/slab.o",
		directory: "mm",
		mode:      "y",
	}); !reflect.DeepEqual(got, []string{"-D__KCSAN_INSTRUMENT_BARRIERS__"}) {
		t.Fatalf("barrier-only KCSAN flags = %#v", got)
	}
	wantForced := append(append([]string{}, wantInstrumented...), "-D__KCSAN_INSTRUMENT_BARRIERS__")
	if got := kcsanKbuildFlags(config, settings, &resolvedKbuildObject{
		object:    "mm/forced.o",
		directory: "mm",
		mode:      "y",
	}); !reflect.DeepEqual(got, wantForced) {
		t.Fatalf("forced KCSAN flags mismatch\nwant: %#v\n got: %#v", wantForced, got)
	}
}

func TestKcsanBarrierFirstSelectorWins(t *testing.T) {
	config := resolvedConfigValues(map[string]string{"CONFIG_KCSAN": "y"})
	object := &resolvedKbuildObject{object: "mm/test.o", directory: "mm", mode: "y"}
	for _, test := range []struct {
		name       string
		object     string
		directory  string
		instrument bool
	}{
		{name: "object n overrides directory y", object: "n", directory: "y"},
		{name: "object y overrides directory n", object: "y", directory: "n", instrument: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := []kbuildObjectSetting{
				{Name: "KCSAN_SANITIZE", Object: object.object, Value: "n"},
				{Name: "KCSAN_INSTRUMENT_BARRIERS", Object: object.object, Directory: object.directory, Value: test.object},
				{Name: "KCSAN_INSTRUMENT_BARRIERS", Directory: object.directory, Value: test.directory},
			}
			got := slices.Contains(kcsanKbuildFlags(config, settings, object), "-D__KCSAN_INSTRUMENT_BARRIERS__")
			if got != test.instrument {
				t.Fatalf("barrier instrumentation = %t, want %t", got, test.instrument)
			}
		})
	}
}

func TestKcsanKbuildFlagsUseReadBeforeWriteFallback(t *testing.T) {
	config := resolvedConfigValues(map[string]string{
		"CONFIG_KCSAN":             "y",
		"CONFIG_KCSAN_WEAK_MEMORY": "y",
	})
	got := kcsanKbuildFlags(config, nil, &resolvedKbuildObject{object: "main.o", mode: "y"})
	want := []string{
		"-fsanitize=thread",
		"-fno-optimize-sibling-calls",
		"-mllvm",
		"-tsan-instrument-read-before-write=1",
		"-mllvm",
		"-tsan-distinguish-volatile=1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback KCSAN flags mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

func TestUbsanKbuildFlagsModelMaintainedKernelLines(t *testing.T) {
	object := &resolvedKbuildObject{object: "lib/test.o", directory: "lib", mode: "m"}
	common := map[string]string{
		"CONFIG_UBSAN":              "y",
		"CONFIG_UBSAN_ALIGNMENT":    "y",
		"CONFIG_UBSAN_ARRAY_BOUNDS": "y",
		"CONFIG_UBSAN_BOOL":         "y",
		"CONFIG_UBSAN_ENUM":         "y",
		"CONFIG_UBSAN_SHIFT":        "y",
		"CONFIG_UBSAN_TRAP":         "y",
	}

	v612 := resolvedConfigValues(common)
	v612.Effective["CONFIG_UBSAN_SIGNED_WRAP"] = "y"
	want612 := []string{
		"-fsanitize=alignment",
		"-fsanitize=array-bounds",
		"-fsanitize=shift",
		"-fsanitize=bool",
		"-fsanitize=enum",
		"-fsanitize-trap=undefined",
		"-fsanitize=signed-integer-overflow",
	}
	if got := ubsanKbuildFlags(v612, nil, object); !reflect.DeepEqual(got, want612) {
		t.Fatalf("Linux 6.12 UBSAN flags mismatch\nwant: %#v\n got: %#v", want612, got)
	}

	v618 := resolvedConfigValues(common)
	v618.Effective["CONFIG_UBSAN_INTEGER_WRAP"] = "y"
	want618 := append(append([]string{}, want612[:len(want612)-1]...),
		"-DINTEGER_WRAP",
		"-fsanitize-undefined-ignore-overflow-pattern=all",
		"-fsanitize=signed-integer-overflow",
		"-fsanitize=unsigned-integer-overflow",
		"-fsanitize=implicit-signed-integer-truncation",
		"-fsanitize=implicit-unsigned-integer-truncation",
		"-fsanitize-ignorelist=$(srctree)/scripts/integer-wrap-ignore.scl",
	)
	if got := ubsanKbuildFlags(v618, nil, object); !reflect.DeepEqual(got, want618) {
		t.Fatalf("Linux 6.18 UBSAN flags mismatch\nwant: %#v\n got: %#v", want618, got)
	}
}

func TestUbsanKbuildFlagsHonorSelectorPrecedence(t *testing.T) {
	config := resolvedConfigValues(map[string]string{
		"CONFIG_UBSAN":             "y",
		"CONFIG_UBSAN_BOOL":        "y",
		"CONFIG_UBSAN_SIGNED_WRAP": "y",
	})
	settings := []kbuildObjectSetting{
		{Name: "UBSAN_SANITIZE", Directory: "lib", Value: "n"},
		{Name: "UBSAN_SIGNED_WRAP", Directory: "lib", Object: "lib/wrap.o", Value: "y"},
		{Name: "UBSAN_SANITIZE", Directory: "lib", Object: "lib/none.o", Value: "n"},
	}
	if got := ubsanKbuildFlags(config, settings, &resolvedKbuildObject{
		object:    "lib/default.o",
		directory: "lib",
		mode:      "y",
	}); len(got) != 0 {
		t.Fatalf("directory UBSAN opt-out flags = %#v, want none", got)
	}
	if got := ubsanKbuildFlags(config, settings, &resolvedKbuildObject{
		object:    "lib/wrap.o",
		directory: "lib",
		mode:      "y",
	}); !reflect.DeepEqual(got, []string{"-fsanitize=signed-integer-overflow"}) {
		t.Fatalf("object wrap override flags = %#v", got)
	}
	if got := ubsanKbuildFlags(config, settings, &resolvedKbuildObject{
		object:    "lib/none.o",
		directory: "lib",
		mode:      "y",
	}); len(got) != 0 {
		t.Fatalf("object UBSAN opt-out flags = %#v, want none", got)
	}
}

func TestUbsanWrapFirstSelectorWins(t *testing.T) {
	config := resolvedConfigValues(map[string]string{
		"CONFIG_UBSAN":             "y",
		"CONFIG_UBSAN_SIGNED_WRAP": "y",
	})
	object := &resolvedKbuildObject{object: "lib/test.o", directory: "lib", mode: "y"}
	for _, test := range []struct {
		name              string
		objectWrap        string
		objectSanitize    string
		directoryWrap     string
		directorySanitize string
		instrument        bool
	}{
		{name: "object wrap n wins", objectWrap: "n", objectSanitize: "y", directoryWrap: "y", directorySanitize: "y"},
		{name: "object wrap y wins", objectWrap: "y", objectSanitize: "n", directoryWrap: "n", directorySanitize: "n", instrument: true},
		{name: "object sanitize n precedes directory wrap", objectSanitize: "n", directoryWrap: "y", directorySanitize: "y"},
		{name: "object sanitize y precedes directory wrap", objectSanitize: "y", directoryWrap: "n", directorySanitize: "n", instrument: true},
		{name: "directory wrap y precedes directory sanitize", directoryWrap: "y", directorySanitize: "n", instrument: true},
		{name: "directory wrap n precedes directory sanitize", directoryWrap: "n", directorySanitize: "y"},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := []kbuildObjectSetting{
				{Name: "UBSAN_SIGNED_WRAP", Object: object.object, Directory: object.directory, Value: test.objectWrap},
				{Name: "UBSAN_SANITIZE", Object: object.object, Directory: object.directory, Value: test.objectSanitize},
				{Name: "UBSAN_SIGNED_WRAP", Directory: object.directory, Value: test.directoryWrap},
				{Name: "UBSAN_SANITIZE", Directory: object.directory, Value: test.directorySanitize},
			}
			got := slices.Contains(ubsanKbuildFlags(config, settings, object), "-fsanitize=signed-integer-overflow")
			if got != test.instrument {
				t.Fatalf("wrap instrumentation = %t, want %t", got, test.instrument)
			}
		})
	}
}

func TestSanitizerDefaultsExcludeArm64NvheMembers(t *testing.T) {
	object := &resolvedKbuildObject{
		object:    "arch/arm64/kvm/hyp/nvhe/switch.nvhe.o",
		directory: "arch/arm64/kvm/hyp/nvhe",
		mode:      "y",
	}
	kasan := resolvedConfigValues(map[string]string{
		"CONFIG_CC_HAS_KASAN_MEMINTRINSIC_PREFIX": "y",
		"CONFIG_KASAN":         "y",
		"CONFIG_KASAN_GENERIC": "y",
	})
	if got := kasanKbuildFlags(kasan, nil, object); len(got) != 0 {
		t.Fatalf("nVHE member received default KASAN flags: %#v", got)
	}
	kcsan := resolvedConfigValues(map[string]string{"CONFIG_KCSAN": "y"})
	if got := kcsanKbuildFlags(kcsan, nil, object); len(got) != 0 {
		t.Fatalf("nVHE member received default KCSAN flags: %#v", got)
	}
	ubsan := resolvedConfigValues(map[string]string{
		"CONFIG_UBSAN":      "y",
		"CONFIG_UBSAN_BOOL": "y",
	})
	if got := ubsanKbuildFlags(ubsan, nil, object); len(got) != 0 {
		t.Fatalf("nVHE member received default UBSAN flags: %#v", got)
	}
	settings := []kbuildObjectSetting{{
		Name:      "UBSAN_SANITIZE",
		Directory: object.directory,
		Value:     "y",
	}}
	if got := ubsanKbuildFlags(ubsan, settings, object); !reflect.DeepEqual(got, []string{"-fsanitize=bool"}) {
		t.Fatalf("nVHE UBSAN directory opt-in flags = %#v", got)
	}
}

func resolvedConfigValues(values map[string]string) *ResolvedConfig {
	effective := map[string]string{}
	for key, value := range values {
		effective[key] = value
	}
	return &ResolvedConfig{Effective: effective}
}
