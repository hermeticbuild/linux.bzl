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
