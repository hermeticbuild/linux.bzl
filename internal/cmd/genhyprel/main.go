// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	hypSectionPrefix       = ".hyp"
	hypRelocSection        = ".hyp.reloc"
	hypSectionSymbolPrefix = "__hyp_section_"

	rAArch64Abs64            = 257
	rAArch64Abs32            = 258
	rAArch64Prel64           = 260
	rAArch64Prel32           = 261
	rAArch64Prel16           = 262
	rAArch64LdPrelLo19       = 273
	rAArch64AdrPrelLo21      = 274
	rAArch64AdrPrelPgHi21    = 275
	rAArch64AdrPrelPgHi21NC  = 276
	rAArch64AddAbsLo12NC     = 277
	rAArch64Ldst8AbsLo12NC   = 278
	rAArch64Tstbr14          = 279
	rAArch64Condbr19         = 280
	rAArch64Jump26           = 282
	rAArch64Call26           = 283
	rAArch64Ldst16AbsLo12NC  = 284
	rAArch64Ldst32AbsLo12NC  = 285
	rAArch64Ldst64AbsLo12NC  = 286
	rAArch64MovwPrelG0       = 287
	rAArch64MovwPrelG0NC     = 288
	rAArch64MovwPrelG1       = 289
	rAArch64MovwPrelG1NC     = 290
	rAArch64MovwPrelG2       = 291
	rAArch64MovwPrelG2NC     = 292
	rAArch64MovwPrelG3       = 293
	rAArch64Ldst128AbsLo12NC = 299
	rAArch64Plt32            = 314
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <elf_input>\n", os.Args[0])
		os.Exit(2)
	}
	if err := emitHypRelocs(os.Stdout, os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "genhyprel: %v\n", err)
		os.Exit(1)
	}
}

func emitHypRelocs(w io.Writer, path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if f.Class != elf.ELFCLASS64 {
		return fmt.Errorf("%s: expected ELF64, got %s", path, f.Class)
	}
	if f.Type != elf.ET_REL {
		return fmt.Errorf("%s: expected relocatable object, got %s", path, f.Type)
	}
	if f.Machine != elf.EM_AARCH64 {
		return fmt.Errorf("%s: expected AArch64 object, got %s", path, f.Machine)
	}

	fmt.Fprintf(w, ".data\n.pushsection %s, \"a\"\n", hypRelocSection)
	var relocOffset uint64
	for _, section := range f.Sections {
		switch section.Type {
		case elf.SHT_REL:
			return fmt.Errorf("%s: unexpected SHT_REL section %q", path, section.Name)
		case elf.SHT_RELA:
			if err := emitRelaSection(w, f, section, &relocOffset); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	fmt.Fprint(w, ".popsection\n")
	return nil
}

func emitRelaSection(w io.Writer, f *elf.File, section *elf.Section, relocOffset *uint64) error {
	if int(section.Info) >= len(f.Sections) {
		return fmt.Errorf("RELA section %q references missing section index %d", section.Name, section.Info)
	}
	original := f.Sections[section.Info]
	if !strings.HasPrefix(original.Name, hypSectionPrefix) {
		return nil
	}
	fmt.Fprintf(w, ".global %s%s\n", hypSectionSymbolPrefix, original.Name)

	relocs, err := relaEntries(f, section)
	if err != nil {
		return err
	}
	for _, rela := range relocs {
		if rela.Off >= original.Size {
			return fmt.Errorf("relocation in %q has offset 0x%x outside section size 0x%x", section.Name, rela.Off, original.Size)
		}
		typ := uint32(rela.Info)
		switch typ {
		case rAArch64Abs64:
			fmt.Fprint(w, ".word 0\n")
			fmt.Fprintf(w, ".reloc %d, R_AARCH64_PREL32, %s%s + 0x%x\n",
				*relocOffset, hypSectionSymbolPrefix, original.Name, rela.Off)
			*relocOffset += 4
		case rAArch64Abs32,
			rAArch64Prel64,
			rAArch64Prel32,
			rAArch64Prel16,
			rAArch64Plt32,
			rAArch64LdPrelLo19,
			rAArch64AdrPrelLo21,
			rAArch64AdrPrelPgHi21,
			rAArch64AdrPrelPgHi21NC,
			rAArch64AddAbsLo12NC,
			rAArch64Ldst8AbsLo12NC,
			rAArch64Ldst16AbsLo12NC,
			rAArch64Ldst32AbsLo12NC,
			rAArch64Ldst64AbsLo12NC,
			rAArch64Ldst128AbsLo12NC,
			rAArch64Tstbr14,
			rAArch64Condbr19,
			rAArch64Jump26,
			rAArch64Call26,
			rAArch64MovwPrelG0,
			rAArch64MovwPrelG0NC,
			rAArch64MovwPrelG1,
			rAArch64MovwPrelG1NC,
			rAArch64MovwPrelG2,
			rAArch64MovwPrelG2NC,
			rAArch64MovwPrelG3:
		default:
			return fmt.Errorf("unexpected RELA type %d in %q", typ, section.Name)
		}
	}
	return nil
}

func relaEntries(f *elf.File, section *elf.Section) ([]elf.Rela64, error) {
	data, err := section.Data()
	if err != nil {
		return nil, err
	}
	if len(data)%24 != 0 {
		return nil, fmt.Errorf("RELA section %q has non-entry-aligned size %d", section.Name, len(data))
	}
	relocs := make([]elf.Rela64, 0, len(data)/24)
	reader := bytes.NewReader(data)
	for reader.Len() != 0 {
		var rela elf.Rela64
		if err := binary.Read(reader, f.ByteOrder, &rela); err != nil {
			return nil, err
		}
		relocs = append(relocs, rela)
	}
	return relocs, nil
}
