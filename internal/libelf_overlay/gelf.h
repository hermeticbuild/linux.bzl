#ifndef LINUX_BZL_GELF_H
#define LINUX_BZL_GELF_H

#include "../lib/gelf.h"

extern GElf_Sym *gelf_getsymshndx __P((Elf_Data *__symdata, Elf_Data *__shndxdata, int __ndx, GElf_Sym *__dst, Elf32_Word *__shndx));
extern int gelf_update_symshndx __P((Elf_Data *__symdata, Elf_Data *__shndxdata, int __ndx, GElf_Sym *__src, Elf32_Word __shndx));

#endif
