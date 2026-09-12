/* Independently compiled by the selected Linux compiler, outside its scanner and probe
 * pipeline. Offsets keep false results file-backed in the measurement ELF. */
const unsigned char measured_attribute_deprecated = 1 + (__has_attribute(deprecated) > 1);
const unsigned char measured_attribute_unknown = 1 + (__has_attribute(linux_bzl_unrecognized_attribute) != 0);
const unsigned char measured_builtin_dynamic_object_size = 1 + (__has_builtin(__builtin_dynamic_object_size) != 0);
const unsigned char measured_builtin_unknown = 1 + (__has_builtin(__builtin_linux_bzl_unrecognized) != 0);

/* A self-reference is unavailable for further expansion after argument
 * prescan. The enum supplies its final C value without any compiler table. */
enum { MAPPED_SELF_VALUE = 17 };
#define MAPPED_SELF_VALUE MAPPED_SELF_VALUE + 1
#define MAPPED_IDENTITY(x) x
const unsigned char measured_self_reference_prescan = MAPPED_IDENTITY(MAPPED_SELF_VALUE);
