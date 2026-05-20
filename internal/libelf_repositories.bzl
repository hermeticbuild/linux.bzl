"""External repositories required by linux.bzl host tools."""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

_LIBELF_PATCH_CMDS = [
    """cat > lib/config.h <<'EOF'
#include <stdint.h>
#define STDC_HEADERS 1
#define ENABLE_EXTENDED_FORMAT 1
#define ENABLE_SANITY_CHECKS 1
#define HAVE_AR_H 1
#define HAVE_ELF_H 1
#define HAVE_FCNTL_H 1
#define HAVE_FTRUNCATE 1
#define HAVE_GETPAGESIZE 1
#define HAVE_LINK_H 1
#define HAVE_MEMCMP 1
#define HAVE_MEMCPY 1
#define HAVE_MEMMOVE 1
#define HAVE_MEMSET 1
#define HAVE_MMAP 1
#define HAVE_STDINT_H 1
#define HAVE_UNISTD_H 1
#define __LIBELF64 1
#define __LIBELF64_LINUX 1
#define __LIBELF_GNU_SYMBOL_VERSIONS 1
#define __LIBELF_HEADER_ELF_H <libelf/elf_repl.h>
#define __LIBELF_SYMBOL_VERSIONS 1
#define __libelf_i16_t int16_t
#define __libelf_i32_t int32_t
#define __libelf_i64_t int64_t
#define __libelf_u16_t uint16_t
#define __libelf_u32_t uint32_t
#define __libelf_u64_t uint64_t
#define SIZEOF_INT 4
#define SIZEOF_LONG 8
#define SIZEOF_LONG_LONG 8
#define SIZEOF_SHORT 2
EOF""",
    """cat > lib/sys_elf.h <<'EOF'
#ifndef LIBELF_SYS_ELF_H
#define LIBELF_SYS_ELF_H

#include <stdint.h>

#define __LIBELF64 1
#define __LIBELF64_LINUX 1
#define __LIBELF_GNU_SYMBOL_VERSIONS 1
#define __LIBELF_HEADER_ELF_H <libelf/elf_repl.h>
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
EOF""",
    """cat >> lib/gelf.h <<'EOF'

extern GElf_Sym *gelf_getsymshndx __P((Elf_Data *__symdata, Elf_Data *__shndxdata, int __ndx, GElf_Sym *__dst, Elf32_Word *__shndx));
extern int gelf_update_symshndx __P((Elf_Data *__symdata, Elf_Data *__shndxdata, int __ndx, GElf_Sym *__src, Elf32_Word __shndx));
EOF
cat > lib/gelf_shndx_compat.c <<'EOF'
#include <gelf.h>
#include <libelf.h>
#include <stddef.h>

static int get_shndx_slot(Elf_Data *data, int ndx, Elf32_Word **slot) {
    size_t offset;

    if (!data || !slot || ndx < 0 || !data->d_buf) {
        return 0;
    }
    if (data->d_type != ELF_T_WORD) {
        return 0;
    }

    offset = (size_t)ndx * sizeof(Elf32_Word);
    if (offset + sizeof(Elf32_Word) > data->d_size) {
        return 0;
    }

    *slot = (Elf32_Word *)((char *)data->d_buf + offset);
    return 1;
}

GElf_Sym *gelf_getsymshndx(Elf_Data *symdata, Elf_Data *shndxdata, int ndx,
                           GElf_Sym *dst, Elf32_Word *shndx) {
    GElf_Sym *sym = gelf_getsym(symdata, ndx, dst);
    Elf32_Word *slot = NULL;

    if (!sym) {
        return NULL;
    }

    if (shndx) {
        if (shndxdata) {
            if (!get_shndx_slot(shndxdata, ndx, &slot)) {
                return NULL;
            }
            *shndx = *slot;
        } else {
            *shndx = sym->st_shndx;
        }
    }

    return sym;
}

int gelf_update_symshndx(Elf_Data *symdata, Elf_Data *shndxdata, int ndx,
                         GElf_Sym *src, Elf32_Word shndx) {
    Elf32_Word *slot = NULL;

    if (!gelf_update_sym(symdata, ndx, src)) {
        return 0;
    }

    if (shndxdata) {
        if (!get_shndx_slot(shndxdata, ndx, &slot)) {
            return 0;
        }
        *slot = shndx;
        elf_flagdata(shndxdata, ELF_C_SET, ELF_F_DIRTY);
    }

    return 1;
}
EOF""",
    "mkdir -p include/libelf && cp lib/libelf.h lib/gelf.h lib/nlist.h lib/sys_elf.h include/ && cp lib/sys_elf.h include/elf.h && cp lib/libelf.h lib/gelf.h lib/nlist.h lib/sys_elf.h lib/elf_repl.h include/libelf/",
]

def _linux_bzl_libelf_impl(mctx):
    http_archive(
        name = "libelf",
        build_file = Label("//:libelf.BUILD.bazel"),
        integrity = "sha256-WRqbTsgcHyBCqXqmBWTgy3nQQcUvqnQWrLOLyVvSx20=",
        patch_cmds = _LIBELF_PATCH_CMDS,
        strip_prefix = "libelf-0.8.13",
        urls = [
            "https://www.mirrorservice.org/sites/ftp.netbsd.org/pub/pkgsrc/distfiles/libelf-0.8.13.tar.gz",
            "https://fossies.org/linux/misc/old/libelf-0.8.13.tar.gz",
        ],
    )
    return mctx.extension_metadata(reproducible = True)

linux_bzl_libelf = module_extension(
    implementation = _linux_bzl_libelf_impl,
)
