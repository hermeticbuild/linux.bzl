/* Literal defined expressions are outside the ordinary prefix inventory's
 * #ifdef/#ifndef hints. The reached-header query must resolve this first guard
 * before the source scanner can enter the independently guarded second file. */
#if !defined(__linux_bzl_guard_round_a)
#include "mapped_guard_b.h"
#else
#error mapped guard round A must not be predefined
#endif
