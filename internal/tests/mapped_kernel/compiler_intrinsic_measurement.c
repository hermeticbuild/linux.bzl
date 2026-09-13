/* Independently compiled by the selected Linux compiler, outside its scanner and probe
 * pipeline. Offsets keep false results file-backed in the measurement ELF. */
const unsigned char measured_attribute_deprecated = 1 + (__has_attribute(deprecated) > 1);
const unsigned char measured_attribute_unknown = 1 + (__has_attribute(linux_bzl_unrecognized_attribute) != 0);
const unsigned char measured_builtin_dynamic_object_size = 1 + (__has_builtin(__builtin_dynamic_object_size) != 0);
const unsigned char measured_builtin_unknown = 1 + (__has_builtin(__builtin_linux_bzl_unrecognized) != 0);

/* Match only the three actual expansions in smoke.c, independently of its
 * scanner and supplemental probes. No compiler-family expectation is encoded. */
const unsigned char measured_counter_first = 17 + __COUNTER__;
const unsigned char measured_counter_condition = 31 + (__COUNTER__ % 2 != 0);
const unsigned char measured_counter_last = 41 + __COUNTER__;

/* A self-reference is unavailable for further expansion after argument
 * prescan. The enum supplies its final C value without any compiler table. */
enum { MAPPED_SELF_VALUE = 17 };
#define MAPPED_SELF_VALUE MAPPED_SELF_VALUE + 1
#define MAPPED_IDENTITY(x) x
const unsigned char measured_self_reference_prescan = MAPPED_IDENTITY(MAPPED_SELF_VALUE);

/* Both operands become unavailable during prescan. Pasting creates a fresh
 * identifier, whose own self-reference is suppressed only after expansion. */
enum { MAPPED_LEFTMAPPED_RIGHT = 19 };
#define MAPPED_LEFT MAPPED_LEFT
#define MAPPED_RIGHT MAPPED_RIGHT
#define MAPPED_LEFTMAPPED_RIGHT MAPPED_LEFTMAPPED_RIGHT + 2
#define MAPPED_RAW_PASTE(a, b) a ## b
#define MAPPED_PASTE(a, b) MAPPED_RAW_PASTE(a, b)
const unsigned char measured_fresh_paste = MAPPED_PASTE(MAPPED_LEFT, MAPPED_RIGHT);
