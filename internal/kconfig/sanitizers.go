package kconfig

import "strings"

func sanitizerKbuildFlags(config *ResolvedConfig, settings []kbuildObjectSetting, object *resolvedKbuildObject) []resolvedKbuildFlag {
	if object == nil {
		return nil
	}
	flags := append([]string{}, kasanKbuildFlags(config, settings, object)...)
	flags = append(flags, ubsanKbuildFlags(config, settings, object)...)
	flags = append(flags, kcsanKbuildFlags(config, settings, object)...)
	if len(flags) == 0 {
		return nil
	}
	return []resolvedKbuildFlag{{
		language: "c",
		values:   flags,
	}}
}

func kasanKbuildFlags(config *ResolvedConfig, settings []kbuildObjectSetting, object *resolvedKbuildObject) []string {
	if !configYes(config, "CONFIG_KASAN") || configYes(config, "CONFIG_KASAN_HW_TAGS") {
		return nil
	}
	objectValue, directoryValue := kbuildObjectSettingValues(settings, object, "KASAN_SANITIZE")
	explicitValue, _ := kbuildObjectSettingValues(settings, object, "CFLAGS_KASAN")
	if !firstKbuildSettingEnabled(false, explicitValue) &&
		!firstKbuildSettingEnabled(sanitizerDefaultEnabled(object), objectValue, directoryValue) {
		if configYes(config, "CONFIG_CC_HAS_KASAN_MEMINTRINSIC_PREFIX") {
			return nil
		}
		return []string{"-fno-builtin"}
	}

	shadowOffset := config.Value("CONFIG_KASAN_SHADOW_OFFSET")
	stack := "0"
	if configYes(config, "CONFIG_KASAN_STACK") {
		stack = "1"
	}
	if configYes(config, "CONFIG_KASAN_GENERIC") {
		callThreshold := "0"
		if configYes(config, "CONFIG_KASAN_INLINE") {
			callThreshold = "10000"
		}
		flags := []string{
			"-fsanitize=kernel-address",
		}
		if shadowOffset != "" && shadowOffset != "n" {
			flags = append(flags,
				"-mllvm",
				"-asan-mapping-offset="+shadowOffset,
			)
		}
		flags = append(flags,
			"-mllvm",
			"-asan-instrumentation-with-call-threshold="+callThreshold,
			"-mllvm",
			"-asan-stack="+stack,
			"-mllvm",
			"-asan-instrument-allocas=1",
			"-mllvm",
			"-asan-globals=1",
			"-mllvm",
			"-asan-kernel-mem-intrinsic-prefix=1",
		)
		return flags
	}
	if configYes(config, "CONFIG_KASAN_SW_TAGS") {
		flags := []string{"-fsanitize=kernel-hwaddress"}
		if configYes(config, "CONFIG_KASAN_INLINE") {
			if shadowOffset != "" && shadowOffset != "n" {
				flags = append(flags,
					"-mllvm",
					"-hwasan-mapping-offset="+shadowOffset,
				)
			}
		} else {
			flags = append(flags,
				"-mllvm",
				"-hwasan-instrument-with-calls=1",
			)
		}
		flags = append(flags,
			"-mllvm",
			"-hwasan-instrument-stack="+stack,
			"-mllvm",
			"-hwasan-use-short-granules=0",
			"-mllvm",
			"-hwasan-inline-all-checks=0",
		)
		if configYes(config, "CONFIG_CC_HAS_KASAN_MEMINTRINSIC_PREFIX") {
			flags = append(flags,
				"-mllvm",
				"-hwasan-kernel-mem-intrinsic-prefix=1",
			)
		}
		return flags
	}
	return nil
}

func ubsanKbuildFlags(config *ResolvedConfig, settings []kbuildObjectSetting, object *resolvedKbuildObject) []string {
	if !configYes(config, "CONFIG_UBSAN") {
		return nil
	}
	objectSanitize, directorySanitize := kbuildObjectSettingValues(settings, object, "UBSAN_SANITIZE")
	flags := []string{}
	if firstKbuildSettingEnabled(sanitizerDefaultEnabled(object), objectSanitize, directorySanitize) {
		for _, option := range []struct {
			symbol string
			flag   string
		}{
			{"CONFIG_UBSAN_ALIGNMENT", "-fsanitize=alignment"},
			{"CONFIG_UBSAN_BOUNDS_STRICT", "-fsanitize=bounds-strict"},
			{"CONFIG_UBSAN_ARRAY_BOUNDS", "-fsanitize=array-bounds"},
			{"CONFIG_UBSAN_LOCAL_BOUNDS", "-fsanitize=local-bounds"},
			{"CONFIG_UBSAN_SHIFT", "-fsanitize=shift"},
			{"CONFIG_UBSAN_DIV_ZERO", "-fsanitize=integer-divide-by-zero"},
			{"CONFIG_UBSAN_UNREACHABLE", "-fsanitize=unreachable"},
			{"CONFIG_UBSAN_BOOL", "-fsanitize=bool"},
			{"CONFIG_UBSAN_ENUM", "-fsanitize=enum"},
		} {
			if configYes(config, option.symbol) {
				flags = append(flags, option.flag)
			}
		}
		if configYes(config, "CONFIG_UBSAN_TRAP") {
			flags = append(flags, "-fsanitize-trap=undefined")
		}
	}

	objectWrap, directoryWrap := kbuildObjectSettingValues(settings, object, "UBSAN_SIGNED_WRAP")
	if configYes(config, "CONFIG_UBSAN_SIGNED_WRAP") &&
		firstKbuildSettingEnabled(sanitizerDefaultEnabled(object), objectWrap, objectSanitize, directoryWrap, directorySanitize) {
		flags = append(flags, "-fsanitize=signed-integer-overflow")
	}

	objectWrap, directoryWrap = kbuildObjectSettingValues(settings, object, "UBSAN_INTEGER_WRAP")
	if configYes(config, "CONFIG_UBSAN_INTEGER_WRAP") &&
		firstKbuildSettingEnabled(sanitizerDefaultEnabled(object), objectWrap, objectSanitize, directoryWrap, directorySanitize) {
		flags = append(flags,
			"-DINTEGER_WRAP",
			"-fsanitize-undefined-ignore-overflow-pattern=all",
			"-fsanitize=signed-integer-overflow",
			"-fsanitize=unsigned-integer-overflow",
			"-fsanitize=implicit-signed-integer-truncation",
			"-fsanitize=implicit-unsigned-integer-truncation",
			"-fsanitize-ignorelist=$(srctree)/scripts/integer-wrap-ignore.scl",
		)
	}
	return flags
}

func kcsanKbuildFlags(config *ResolvedConfig, settings []kbuildObjectSetting, object *resolvedKbuildObject) []string {
	if !configYes(config, "CONFIG_KCSAN") {
		return nil
	}
	objectSanitize, directorySanitize := kbuildObjectSettingValues(settings, object, "KCSAN_SANITIZE")
	explicitValue, _ := kbuildObjectSettingValues(settings, object, "CFLAGS_KCSAN")
	flags := []string{}
	if firstKbuildSettingEnabled(false, explicitValue) ||
		firstKbuildSettingEnabled(sanitizerDefaultEnabled(object), objectSanitize, directorySanitize) {
		flags = append(flags,
			"-fsanitize=thread",
			"-fno-optimize-sibling-calls",
			"-mllvm",
		)
		if configYes(config, "CONFIG_CC_HAS_TSAN_COMPOUND_READ_BEFORE_WRITE") {
			flags = append(flags, "-tsan-compound-read-before-write=1")
		} else {
			flags = append(flags, "-tsan-instrument-read-before-write=1")
		}
		flags = append(flags,
			"-mllvm",
			"-tsan-distinguish-volatile=1",
		)
		if !configYes(config, "CONFIG_KCSAN_WEAK_MEMORY") {
			flags = append(flags,
				"-mllvm",
				"-tsan-instrument-func-entry-exit=0",
			)
		}
	}

	objectBarriers, directoryBarriers := kbuildObjectSettingValues(settings, object, "KCSAN_INSTRUMENT_BARRIERS")
	if firstKbuildSettingEnabled(false, objectBarriers, directoryBarriers) {
		flags = append(flags, "-D__KCSAN_INSTRUMENT_BARRIERS__")
	}
	return flags
}

func kbuildObjectSettingValues(settings []kbuildObjectSetting, object *resolvedKbuildObject, name string) (string, string) {
	objectValue := ""
	directoryValue := ""
	for _, setting := range settings {
		if setting.Name != name {
			continue
		}
		if setting.Object == object.object {
			objectValue = setting.Value
		} else if setting.Object == "" && setting.Directory == object.directory {
			directoryValue = setting.Value
		}
	}
	return objectValue, directoryValue
}

func firstKbuildSettingEnabled(defaultValue bool, values ...string) bool {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		return !strings.HasPrefix(value, "n")
	}
	return defaultValue
}

func configYes(config *ResolvedConfig, symbol string) bool {
	return config != nil && config.Value(symbol) == "y"
}

func sanitizerDefaultEnabled(object *resolvedKbuildObject) bool {
	return object != nil && !strings.HasSuffix(object.object, ".nvhe.o")
}
