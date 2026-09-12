/* The fallback shape mirrors Linux's compiler_types.h. A source-owned wrapper
 * must preserve the measured builtin binding without becoming an answer table. */
#ifndef MAPPED_COMPILER_TYPES_H
#define MAPPED_COMPILER_TYPES_H

#ifndef __has_builtin
#define __has_builtin(x) 0
#endif

#define MAPPED_HAS_BUILTIN(name) __has_builtin(name)

#endif
