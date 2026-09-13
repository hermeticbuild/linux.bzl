#include <generated/mapped_family_value.h>
#include "include/linux/mapped_guard_a.h"
#include "include/linux/mapped_compiler_types.h"

/* Stateful expansions must follow source order, including discarded and raw
 * stringified arguments. The test compares against a separate compiler action,
 * without assuming the counter's initial value or increment. */
#ifndef __COUNTER__
#error counter fixture requires a measured compiler counter
#endif
#define MAPPED_COUNTER_DROP(x)
#define MAPPED_COUNTER_RAW(x) #x
const unsigned char mapped_counter_first = 17 + __COUNTER__;
MAPPED_COUNTER_DROP(__COUNTER__)
const char mapped_counter_raw[] = MAPPED_COUNTER_RAW(__COUNTER__);
#if __COUNTER__ % 2 == 0
const unsigned char mapped_counter_condition = 31;
#else
const unsigned char mapped_counter_condition = 32;
#endif
const unsigned char mapped_counter_last = 41 + __COUNTER__;

/* The test reads this byte from the ELF section, not the host-output suffix.
 * The zero-valued source macro requires both reached-header compiler rounds
 * for precise reuse, while leaving the actual compiled value unchanged. */
const unsigned char mapped_generated_header_value = 73 + MAPPED_FAMILY_GENERATED_VALUE + MAPPED_GUARD_ROUND_VALUE;

/* A raw CONFIG token scan cannot see the resulting option name. The family
 * must prove this complete call and conditional before sharing smoke.o. */
#define MAPPED_OPTION(name) CONFIG_ ## name
#if MAPPED_OPTION(USED_FAMILY_OPTION)
#define MAPPED_PASTED_VALUE 1
#else
#define MAPPED_PASTED_VALUE 0
#endif
const unsigned char mapped_pasted_config_value = 91 + MAPPED_PASTED_VALUE;

/* Exercise value-sensitive queries through the selected real compiler and the
 * default supplemental rounds. No compiler-family/version answer is supplied
 * by the test: the emitted bytes cross-check C and preprocessing evaluation. */
#if __has_attribute(deprecated) > 1
#define MAPPED_INTRINSIC_BRANCH 1
#else
#define MAPPED_INTRINSIC_BRANCH 0
#endif
const unsigned char mapped_intrinsic_expression = __has_attribute(deprecated) > 1;
const unsigned char mapped_intrinsic_condition = 113 + MAPPED_INTRINSIC_BRANCH;

/* Keep a distinct call in the same compiler invocation. An unrecognized
 * attribute is a valid measured integer result, not an unavailable answer.
 * Offsets keep both witnesses file-backed even when the comparison is false. */
#if __has_attribute(linux_bzl_unrecognized_attribute) != 0
#define MAPPED_SECOND_INTRINSIC_BRANCH 1
#else
#define MAPPED_SECOND_INTRINSIC_BRANCH 0
#endif
const unsigned char mapped_second_intrinsic_expression = 127 + (__has_attribute(linux_bzl_unrecognized_attribute) != 0);
const unsigned char mapped_second_intrinsic_condition = 149 + MAPPED_SECOND_INTRINSIC_BRANCH;

/* Linux compiler_types.h asks about dynamic object size. Keep a source-owned
 * wrapper in the condition and direct literal calls in C expressions, so both
 * complete-call interpretation and batched discovery are exercised. */
#if MAPPED_HAS_BUILTIN(__builtin_dynamic_object_size) != 0
#define MAPPED_BUILTIN_BRANCH 1
#else
#define MAPPED_BUILTIN_BRANCH 0
#endif
const unsigned char mapped_builtin_expression = 161 + (__has_builtin(__builtin_dynamic_object_size) != 0);
const unsigned char mapped_builtin_condition = 173 + MAPPED_BUILTIN_BRANCH;

#if MAPPED_HAS_BUILTIN(__builtin_linux_bzl_unrecognized) != 0
#define MAPPED_UNKNOWN_BUILTIN_BRANCH 1
#else
#define MAPPED_UNKNOWN_BUILTIN_BRANCH 0
#endif
const unsigned char mapped_unknown_builtin_expression = 181 + (__has_builtin(__builtin_linux_bzl_unrecognized) != 0);
const unsigned char mapped_unknown_builtin_condition = 193 + MAPPED_UNKNOWN_BUILTIN_BRANCH;

#if MAPPED_HAS_BUILTIN(__builtin_dynamic_object_size) != 0 && MAPPED_OPTION(USED_FAMILY_OPTION)
#define MAPPED_BUILTIN_CONFIG_BRANCH 1
#else
#define MAPPED_BUILTIN_CONFIG_BRANCH 0
#endif
const unsigned char mapped_builtin_pasted_config_value = 211 + MAPPED_BUILTIN_CONFIG_BRANCH;

/* A self-reference is unavailable for further expansion after argument
 * prescan. The enum supplies its final C value without any compiler table. */
enum { MAPPED_SELF_VALUE = 17 };
#define MAPPED_SELF_VALUE MAPPED_SELF_VALUE + 1
#define MAPPED_IDENTITY(x) x
const unsigned char mapped_self_reference_prescan = MAPPED_IDENTITY(MAPPED_SELF_VALUE);

/* Both operands become unavailable during prescan. Pasting creates a fresh
 * identifier, whose own self-reference is suppressed only after expansion. */
enum { MAPPED_LEFTMAPPED_RIGHT = 19 };
#define MAPPED_LEFT MAPPED_LEFT
#define MAPPED_RIGHT MAPPED_RIGHT
#define MAPPED_LEFTMAPPED_RIGHT MAPPED_LEFTMAPPED_RIGHT + 2
#define MAPPED_RAW_PASTE(a, b) a ## b
#define MAPPED_PASTE(a, b) MAPPED_RAW_PASTE(a, b)
const unsigned char mapped_fresh_paste = MAPPED_PASTE(MAPPED_LEFT, MAPPED_RIGHT);

int mapped_kernel_smoke(void) {
	return MAPPED_FAMILY_GENERATED_VALUE;
}

/* These are C enumerators, not preprocessor definitions. Complete macro-call
 * analysis must measure their initial compiler bindings. Directive-separated
 * names exceed the three-round first-unknown-only pipeline; opened-file
 * hints must batch them without guessing that any name is undefined. */
enum {
	__MAPPED_HINT_A = 1,
#if 1
	__MAPPED_HINT_B = 2,
#endif
#if 1
	__MAPPED_HINT_C = 3,
#endif
#if 1
	__MAPPED_HINT_D = 4,
#endif
#if 1
	__MAPPED_HINT_E = 5,
#endif
#if 1
	__MAPPED_HINT_F = 6
#endif
};
const unsigned char mapped_entered_hint_value = __MAPPED_HINT_A + __MAPPED_HINT_B + __MAPPED_HINT_C +
	__MAPPED_HINT_D + __MAPPED_HINT_E + __MAPPED_HINT_F;
