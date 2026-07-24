#ifndef LIBELF_SYS_ELF_H
#define LIBELF_SYS_ELF_H

#include <stdint.h>

#define __LIBELF64 1
#define __LIBELF64_LINUX 1
#define __LIBELF_GNU_SYMBOL_VERSIONS 1
#define __LIBELF_HEADER_ELF_H <elf_repl.h>
#define __LIBELF_SYMBOL_VERSIONS 1
#define __libelf_i16_t int16_t
#define __libelf_i32_t int32_t
#define __libelf_i64_t int64_t
#define __libelf_u16_t uint16_t
#define __libelf_u32_t uint32_t
#define __libelf_u64_t uint64_t

#include __LIBELF_HEADER_ELF_H

#ifndef ELF32_FSZ_ADDR
#define ELF32_FSZ_ADDR 4
#define ELF32_FSZ_HALF 2
#define ELF32_FSZ_OFF 4
#define ELF32_FSZ_SWORD 4
#define ELF32_FSZ_WORD 4
#endif

#ifndef STN_UNDEF
#define STN_UNDEF 0
#endif

#ifndef ELF64_FSZ_ADDR
#define ELF64_FSZ_ADDR 8
#define ELF64_FSZ_HALF 2
#define ELF64_FSZ_OFF 8
#define ELF64_FSZ_SWORD 4
#define ELF64_FSZ_WORD 4
#define ELF64_FSZ_SXWORD 8
#define ELF64_FSZ_XWORD 8
#endif

#ifndef ELF64_ST_BIND
#define ELF64_ST_BIND(i) ((i) >> 4)
#define ELF64_ST_TYPE(i) ((i) & 0xf)
#define ELF64_ST_INFO(b, t) (((b) << 4) + ((t) & 0xf))
#endif

#ifndef ELF64_R_SYM
#define ELF64_R_SYM(i) ((Elf64_Xword)(i) >> 32)
#define ELF64_R_TYPE(i) ((i) & 0xffffffffL)
#define ELF64_R_INFO(s, t) (((Elf64_Xword)(s) << 32) + ((t) & 0xffffffffL))
#endif

#ifndef R_X86_64_NONE
#define R_X86_64_NONE 0
#define R_X86_64_64 1
#define R_X86_64_PC32 2
#define R_X86_64_PLT32 4
#define R_X86_64_GOTPCREL 9
#define R_X86_64_32 10
#define R_X86_64_32S 11
#define R_X86_64_PC16 13
#define R_X86_64_PC8 15
#define R_X86_64_PC64 24
#define R_X86_64_GOTPC32 26
#endif

#ifndef R_AARCH64_NONE
#define R_AARCH64_NONE 0
#define R_AARCH64_ABS64 257
#define R_AARCH64_PREL64 260
#endif

#ifndef EF_ARM_EABI_MASK
#define EF_ARM_EABI_MASK 0xff000000
#define EF_ARM_EABI_VERSION(flags) ((flags) & EF_ARM_EABI_MASK)
#endif

#endif
