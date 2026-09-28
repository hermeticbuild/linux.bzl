/* This translation unit is compiled to assembly, then transformed into a
 * header through the same source-owned offsets protocol used by Linux.
 * Keeping a real string initializer makes leaked shell quoting invalid C. */
#if !defined(MAPPED_DEFERRED_ASSEMBLY_CONTEXT) || MAPPED_DEFERRED_ASSEMBLY_CONTEXT != 1
#error generated assembly must use the deferred fixture compiler context
#endif

const char mapped_offsets_modfile[] = KBUILD_MODFILE;

#include "include/linux/mapped_compiler_types.h"

#if __has_attribute(deprecated) > 1
#define MAPPED_OFFSETS_INTRINSIC_BRANCH 1
#else
#define MAPPED_OFFSETS_INTRINSIC_BRANCH 0
#endif

#if MAPPED_HAS_BUILTIN(__builtin_dynamic_object_size) != 0
#define MAPPED_OFFSETS_BUILTIN_BRANCH 1
#else
#define MAPPED_OFFSETS_BUILTIN_BRANCH 0
#endif

#if MAPPED_HAS_BUILTIN(__builtin_linux_bzl_unrecognized) != 0
#define MAPPED_OFFSETS_UNKNOWN_BUILTIN_BRANCH 1
#else
#define MAPPED_OFFSETS_UNKNOWN_BUILTIN_BRANCH 0
#endif

#if CONFIG_USED_FAMILY_OPTION
#define MAPPED_OFFSETS_CONFIG_VALUE 1
#else
#define MAPPED_OFFSETS_CONFIG_VALUE 0
#endif

void mapped_asm_offsets(void)
{
	asm volatile("\n.ascii \"->MAPPED_OFFSETS_EXPRESSION %0 deprecated\"\n"
		     : : "i" (__has_attribute(deprecated) > 1));
	asm volatile("\n.ascii \"->MAPPED_OFFSETS_CONDITION %0 deprecated_branch\"\n"
		     : : "i" (MAPPED_OFFSETS_INTRINSIC_BRANCH));
	asm volatile("\n.ascii \"->MAPPED_OFFSETS_CONFIG %0 used_family_option\"\n"
		     : : "i" (MAPPED_OFFSETS_CONFIG_VALUE));
	asm volatile("\n.ascii \"->MAPPED_OFFSETS_BUILTIN_EXPRESSION %0 dynamic_object_size\"\n"
		     : : "i" (__has_builtin(__builtin_dynamic_object_size) != 0));
	asm volatile("\n.ascii \"->MAPPED_OFFSETS_BUILTIN_CONDITION %0 dynamic_object_size_branch\"\n"
		     : : "i" (MAPPED_OFFSETS_BUILTIN_BRANCH));
	asm volatile("\n.ascii \"->MAPPED_OFFSETS_UNKNOWN_BUILTIN_EXPRESSION %0 unknown_builtin\"\n"
		     : : "i" (__has_builtin(__builtin_linux_bzl_unrecognized) != 0));
	asm volatile("\n.ascii \"->MAPPED_OFFSETS_UNKNOWN_BUILTIN_CONDITION %0 unknown_builtin_branch\"\n"
		     : : "i" (MAPPED_OFFSETS_UNKNOWN_BUILTIN_BRANCH));
}
