package modversions

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"strings"
	"testing"
)

type fixtureSection struct {
	name string
	data []byte
}

func fixtureELF(t *testing.T, width int, order binary.ByteOrder, sections ...fixtureSection) []byte {
	t.Helper()
	encode := func(value any) []byte {
		var b bytes.Buffer
		if err := binary.Write(&b, order, value); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	symtab := []byte{}
	if width == 8 {
		symtab = append(encode(elf.Sym64{}), encode(elf.Sym64{Name: 1, Info: byte(elf.STB_GLOBAL) << 4})...)
	} else {
		symtab = append(encode(elf.Sym32{}), encode(elf.Sym32{Name: 1, Info: byte(elf.STB_GLOBAL) << 4})...)
	}
	strtabIndex := len(sections) + 1
	sections = append(sections, fixtureSection{".strtab", []byte("\x00_printk\x00")}, fixtureSection{".symtab", symtab})
	names := []byte{0}
	nameOffsets := []uint32{}
	for _, s := range append(sections, fixtureSection{name: ".shstrtab"}) {
		nameOffsets = append(nameOffsets, uint32(len(names)))
		names = append(names, []byte(s.name)...)
		names = append(names, 0)
	}
	sections = append(sections, fixtureSection{".shstrtab", names})
	headerSize, sectionSize := 52, 40
	if width == 8 {
		headerSize, sectionSize = 64, 64
	}
	image := make([]byte, headerSize)
	offsets := make([]int, len(sections))
	for i, s := range sections {
		offsets[i] = len(image)
		image = append(image, s.data...)
	}
	shoff := len(image)
	image = append(image, make([]byte, sectionSize)...)
	for i, s := range sections {
		typ, link, entsize := uint32(elf.SHT_PROGBITS), uint32(0), uint64(0)
		if s.name == ".strtab" || s.name == ".shstrtab" {
			typ = uint32(elf.SHT_STRTAB)
		}
		if s.name == ".symtab" {
			typ, link = uint32(elf.SHT_SYMTAB), uint32(strtabIndex)
			entsize = 16
			if width == 8 {
				entsize = 24
			}
		}
		if width == 8 {
			image = append(image, encode(elf.Section64{Name: nameOffsets[i], Type: typ, Off: uint64(offsets[i]), Size: uint64(len(s.data)), Link: link, Addralign: 1, Entsize: entsize})...)
		} else {
			image = append(image, encode(elf.Section32{Name: nameOffsets[i], Type: typ, Off: uint32(offsets[i]), Size: uint32(len(s.data)), Link: link, Addralign: 1, Entsize: uint32(entsize)})...)
		}
	}
	var ident [16]byte
	copy(ident[:], "\x7fELF")
	ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	if order == binary.BigEndian {
		ident[elf.EI_DATA] = byte(elf.ELFDATA2MSB)
	}
	if width == 8 {
		ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
		copy(image, encode(elf.Header64{Ident: ident, Type: uint16(elf.ET_REL), Machine: uint16(elf.EM_X86_64), Version: 1, Shoff: uint64(shoff), Ehsize: uint16(headerSize), Shentsize: uint16(sectionSize), Shnum: uint16(len(sections) + 1), Shstrndx: uint16(len(sections))}))
	} else {
		ident[elf.EI_CLASS] = byte(elf.ELFCLASS32)
		copy(image, encode(elf.Header32{Ident: ident, Type: uint16(elf.ET_REL), Machine: uint16(elf.EM_386), Version: 1, Shoff: uint32(shoff), Ehsize: uint16(headerSize), Shentsize: uint16(sectionSize), Shnum: uint16(len(sections) + 1), Shstrndx: uint16(len(sections))}))
	}
	return image
}

func basicRecords(width int, order binary.ByteOrder) []byte {
	data := make([]byte, 128)
	for i, name := range []string{"module_layout", "_printk"} {
		if width == 8 {
			order.PutUint64(data[i*64:], uint64(i))
		} else {
			order.PutUint32(data[i*64:], uint32(i))
		}
		copy(data[i*64+width:], name)
	}
	return data
}

func TestELFVersionFormatsAndSingleFieldMutation(t *testing.T) {
	for _, width := range []int{4, 8} {
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			for _, format := range []string{"basic", "extended", "both"} {
				t.Run(order.String()+format+string(rune('0'+width)), func(t *testing.T) {
					var sections []fixtureSection
					if format != "extended" {
						sections = append(sections, fixtureSection{"__versions", basicRecords(width, order)})
					}
					if format != "basic" {
						crcs := make([]byte, 8)
						order.PutUint32(crcs[4:], 1)
						sections = append(sections, fixtureSection{"__version_ext_crcs", crcs}, fixtureSection{"__version_ext_names", []byte("module_layout\x00_printk\x00\x00")})
					}
					image := fixtureELF(t, width, order, sections...)
					before := bytes.Clone(image)
					module, err := ReadModule(image)
					if err != nil {
						t.Fatal(err)
					}
					producers := map[string]uint32{"module_layout": 0, "_printk": 1}
					if err := Validate(module, producers); err != nil {
						t.Fatal(err)
					}
					changed, err := WithAlteredCRC(image, "module_layout")
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(image, before) {
						t.Fatal("mutated original module")
					}
					diff := 0
					for i := range image {
						if image[i] != changed[i] {
							diff++
						}
					}
					if diff != 1 {
						t.Fatalf("changed %d bytes, want exactly one low bit", diff)
					}
					record := module.versions["module_layout"]
					var crc uint64
					if record.width == 8 {
						crc = order.Uint64(changed[record.offset:])
					} else {
						crc = uint64(order.Uint32(changed[record.offset:]))
					}
					if crc != 1 {
						t.Fatalf("mutated CRC = %#x", crc)
					}
					if format == "both" {
						if _, err := ReadModule(changed); err == nil {
							t.Fatal("inconsistent dual-format CRC accepted")
						}
					} else {
						bad, err := ReadModule(changed)
						if err != nil {
							t.Fatal(err)
						}
						if err := Validate(bad, producers); err == nil {
							t.Fatal("changed CRC validated")
						}
					}
					if _, err := WithAlteredCRC(image, "absent"); err == nil {
						t.Fatal("missing mutation symbol accepted")
					}
				})
			}
		}
	}
}

func TestMalformedVersionRecords(t *testing.T) {
	order := binary.LittleEndian
	for name, data := range map[string][]byte{
		"truncated":    {0},
		"empty name":   make([]byte, 64),
		"unterminated": append(make([]byte, 8), bytes.Repeat([]byte{'x'}, 56)...),
		"non-u32":      append([]byte{0, 0, 0, 0, 1, 0, 0, 0}, append([]byte("name\x00"), make([]byte, 51)...)...),
		"duplicate":    append(basicRecords(8, order)[:64:64], basicRecords(8, order)[:64]...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseBasic(data, 8, order, 0); err == nil {
				t.Fatal("invalid basic records accepted")
			}
		})
	}
	for name, test := range map[string]struct{ crcs, names []byte }{
		"unaligned CRC":  {[]byte{0}, nil},
		"missing name":   {make([]byte, 4), nil},
		"unterminated":   {make([]byte, 4), []byte("name")},
		"extra name":     {make([]byte, 4), []byte("name\x00extra\x00")},
		"extra padding":  {make([]byte, 4), []byte("name\x00\x00\x00")},
		"duplicate name": {make([]byte, 8), []byte("name\x00name\x00")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseExtended(test.crcs, test.names, order, 0); err == nil {
				t.Fatal("invalid extended records accepted")
			}
		})
	}
	for name, sections := range map[string][]fixtureSection{
		"missing all":       nil,
		"missing names":     {{"__version_ext_crcs", make([]byte, 4)}},
		"missing CRC":       {{"__version_ext_names", []byte("name\x00")}},
		"duplicate section": {{"__versions", basicRecords(8, order)}, {"__versions", basicRecords(8, order)}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadModule(fixtureELF(t, 8, order, sections...)); err == nil {
				t.Fatal("invalid module accepted")
			}
		})
	}
	if _, err := ReadModule([]byte("not ELF")); err == nil {
		t.Fatal("non-ELF accepted")
	}
}

func TestSymversAndRequiredImportValidation(t *testing.T) {
	good := "0x00000000\tmodule_layout\tvmlinux\tEXPORT_SYMBOL\t\n0x00000001\t_printk\tvmlinux\tEXPORT_SYMBOL\t\n"
	producers, err := ParseSymvers(strings.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"\n", "0\tname\towner\tEXPORT_SYMBOL\t\n", "0x100000000\tname\towner\tEXPORT_SYMBOL\t\n", "0x1\tname\towner\tBOGUS\t\n", good + good, "0x1\tname\towner\tEXPORT_SYMBOL\n"} {
		if _, err := ParseSymvers(strings.NewReader(text)); err == nil {
			t.Fatalf("malformed symvers accepted: %q", text)
		}
	}
	if _, err := MergeSymvers(producers, producers); err == nil {
		t.Fatal("ambiguous export ownership accepted")
	}
	module := &Module{versions: map[string]version{"module_layout": {crc: 0}, "_printk": {crc: 1}}, imports: map[string]bool{"_printk": false, "optional": true}}
	if err := Validate(module, producers); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"module_layout", "_printk"} {
		saved := module.versions[name]
		delete(module.versions, name)
		if err := Validate(module, producers); err == nil {
			t.Fatalf("missing %s accepted", name)
		}
		module.versions[name] = saved
	}
	providers, err := ParseSymvers(strings.NewReader("0x2\tdependency\tprovider\tEXPORT_SYMBOL_GPL\t\n"))
	if err != nil {
		t.Fatal(err)
	}
	all, err := MergeSymvers(producers, providers)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(module, all, "dependency"); err == nil {
		t.Fatal("missing explicit dependency CRC accepted")
	}
	module.versions["dependency"] = version{crc: 2}
	module.imports["dependency"] = false
	if err := Validate(module, all, "dependency"); err != nil {
		t.Fatal(err)
	}
	all["dependency"] = 3
	if err := Validate(module, all); err == nil {
		t.Fatal("dependency CRC mismatch accepted")
	}
	module.imports["unresolved_strong"] = false
	if err := Validate(module, producers); err == nil {
		t.Fatal("unresolved strong import accepted")
	}
	delete(module.imports, "unresolved_strong")
	delete(module.versions, "dependency")
	delete(module.imports, "dependency")
	producers["optional"] = 0
	if err := Validate(module, producers); err == nil || !strings.Contains(err.Error(), "optional") {
		t.Fatal("resolved weak import without record accepted")
	}
}

func TestExtendedTableDoesNotFallBackToBasicAndSignedMutationIsRejected(t *testing.T) {
	order := binary.LittleEndian
	for _, missing := range []string{"module_layout", "_printk"} {
		name, crc := "module_layout", uint32(0)
		if missing == "module_layout" {
			name, crc = "_printk", 1
		}
		crcs := make([]byte, 4)
		order.PutUint32(crcs, crc)
		image := fixtureELF(t, 8, order,
			fixtureSection{"__versions", basicRecords(8, order)},
			fixtureSection{"__version_ext_crcs", crcs},
			fixtureSection{"__version_ext_names", []byte(name + "\x00\x00")},
		)
		module, err := ReadModule(image)
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(module, map[string]uint32{"module_layout": 0, "_printk": 1}); err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("ignored basic record masked missing extended %s: %v", missing, err)
		}
		if _, err := WithAlteredCRC(image, missing); err == nil {
			t.Fatal("mutator changed ineffective basic CRC")
		}
	}
	image := fixtureELF(t, 8, order, fixtureSection{"__versions", basicRecords(8, order)})
	signed := append(bytes.Clone(image), []byte("~Module signature appended~\n")...)
	if _, err := ReadModule(signed); err != nil {
		t.Fatalf("signed metadata rejected: %v", err)
	}
	if _, err := WithAlteredCRC(signed, "module_layout"); err == nil {
		t.Fatal("signed mutation accepted")
	}
}
