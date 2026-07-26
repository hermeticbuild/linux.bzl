package main

import (
	"debug/elf"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	r386None  = 0
	r38632    = 1
	r386PC32  = 2
	r386PLT32 = 4
	r38616    = 20
	r386PC16  = 21
	r386PC8   = 23
)

type elf32Symbol struct {
	name  string
	shndx uint16
}

func main() {
	in := flag.String("in", "", "realmode.elf input")
	out := flag.String("out", "", "realmode.relocs output")
	flag.Parse()

	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-in and -out are required")
		os.Exit(2)
	}
	if err := run(*in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "realmoderelocs: %v\n", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	file, err := elf.Open(in)
	if err != nil {
		return err
	}
	defer file.Close()
	if file.Class != elf.ELFCLASS32 {
		return fmt.Errorf("--realmode is only valid for ELF32 files, got %s", file.Class)
	}
	if file.Data != elf.ELFDATA2LSB {
		return fmt.Errorf("unsupported realmode ELF byte order %s", file.Data)
	}

	var relocs16 []uint32
	var relocs32 []uint32
	symbolCache := map[uint32][]elf32Symbol{}
	for _, section := range file.Sections {
		if section.Type != elf.SHT_REL {
			continue
		}
		if int(section.Info) >= len(file.Sections) {
			return fmt.Errorf("%s references invalid target section %d", section.Name, section.Info)
		}
		target := file.Sections[section.Info]
		if target.Flags&elf.SHF_ALLOC == 0 || target.Type == elf.SHT_NOTE {
			continue
		}
		symbols, ok := symbolCache[section.Link]
		if !ok {
			symbols, err = readSymbols(file, section.Link)
			if err != nil {
				return err
			}
			symbolCache[section.Link] = symbols
		}
		data, err := section.Data()
		if err != nil {
			return err
		}
		if len(data)%8 != 0 {
			return fmt.Errorf("%s size %d is not a multiple of Elf32_Rel", section.Name, len(data))
		}
		for off := 0; off < len(data); off += 8 {
			relOffset := binary.LittleEndian.Uint32(data[off:])
			info := binary.LittleEndian.Uint32(data[off+4:])
			symIndex := info >> 8
			relocType := info & 0xff
			if int(symIndex) >= len(symbols) {
				return fmt.Errorf("%s relocation references invalid symbol %d", section.Name, symIndex)
			}
			sym := symbols[symIndex]
			add16, add32, err := classifyRealmodeReloc(relocType, relOffset, sym)
			if err != nil {
				return fmt.Errorf("%s: %w", section.Name, err)
			}
			if add16 {
				relocs16 = append(relocs16, relOffset)
			}
			if add32 {
				relocs32 = append(relocs32, relOffset)
			}
		}
	}

	sort.Slice(relocs16, func(i, j int) bool { return relocs16[i] < relocs16[j] })
	sort.Slice(relocs32, func(i, j int) bool { return relocs32[i] < relocs32[j] })

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	output, err := os.Create(out)
	if err != nil {
		return err
	}
	defer output.Close()
	writeU32(output, uint32(len(relocs16)))
	for _, reloc := range relocs16 {
		writeU32(output, reloc)
	}
	writeU32(output, uint32(len(relocs32)))
	for _, reloc := range relocs32 {
		writeU32(output, reloc)
	}
	return nil
}

func readSymbols(file *elf.File, sectionIndex uint32) ([]elf32Symbol, error) {
	if int(sectionIndex) >= len(file.Sections) {
		return nil, fmt.Errorf("invalid symbol table section %d", sectionIndex)
	}
	section := file.Sections[sectionIndex]
	if section.Type != elf.SHT_SYMTAB && section.Type != elf.SHT_DYNSYM {
		return nil, fmt.Errorf("%s is not a symbol table", section.Name)
	}
	if int(section.Link) >= len(file.Sections) {
		return nil, fmt.Errorf("%s references invalid string table %d", section.Name, section.Link)
	}
	stringsData, err := file.Sections[section.Link].Data()
	if err != nil {
		return nil, err
	}
	data, err := section.Data()
	if err != nil {
		return nil, err
	}
	if len(data)%16 != 0 {
		return nil, fmt.Errorf("%s size %d is not a multiple of Elf32_Sym", section.Name, len(data))
	}
	symbols := make([]elf32Symbol, 0, len(data)/16)
	for off := 0; off < len(data); off += 16 {
		nameOffset := binary.LittleEndian.Uint32(data[off:])
		shndx := binary.LittleEndian.Uint16(data[off+14:])
		name, err := stringAt(stringsData, nameOffset)
		if err != nil {
			return nil, fmt.Errorf("%s symbol at %d: %w", section.Name, off, err)
		}
		symbols = append(symbols, elf32Symbol{name: name, shndx: shndx})
	}
	return symbols, nil
}

func stringAt(data []byte, offset uint32) (string, error) {
	if int(offset) >= len(data) {
		return "", fmt.Errorf("string offset %d out of range", offset)
	}
	end := int(offset)
	for end < len(data) && data[end] != 0 {
		end++
	}
	return string(data[int(offset):end]), nil
}

func classifyRealmodeReloc(relocType uint32, offset uint32, sym elf32Symbol) (bool, bool, error) {
	shnAbs := sym.shndx == uint16(elf.SHN_ABS) && !isRelativeSymbol(sym.name)
	switch relocType {
	case r386None, r386PC32, r386PC16, r386PC8, r386PLT32:
		return false, false, nil
	case r38616:
		if shnAbs {
			if isSegmentSymbol(sym.name) {
				return true, false, nil
			}
		} else if !isLinearSymbol(sym.name) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("invalid %s R_386_16 relocation at %#x: %s", absRel(shnAbs), offset, sym.name)
	case r38632:
		if shnAbs {
			if isRelativeSymbol(sym.name) {
				return false, true, nil
			}
		} else {
			if isLinearSymbol(sym.name) {
				return false, true, nil
			}
			return false, false, nil
		}
		return false, false, fmt.Errorf("invalid %s R_386_32 relocation at %#x: %s", absRel(shnAbs), offset, sym.name)
	default:
		return false, false, fmt.Errorf("unsupported relocation type %d at %#x: %s", relocType, offset, sym.name)
	}
}

func isRelativeSymbol(name string) bool {
	return strings.HasPrefix(name, "pa_")
}

func isSegmentSymbol(name string) bool {
	return name == "real_mode_seg"
}

func isLinearSymbol(name string) bool {
	return strings.HasPrefix(name, "pa_")
}

func absRel(shnAbs bool) string {
	if shnAbs {
		return "absolute"
	}
	return "relative"
}

func writeU32(output *os.File, value uint32) {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], value)
	_, _ = output.Write(buf[:])
}
