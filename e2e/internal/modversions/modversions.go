// Package modversions validates the on-disk Linux module version ABI.
// It is test infrastructure, not a replacement for modpost or the kernel loader.
package modversions

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const MaxImageBytes = 64 << 20
const maxSectionBytes = 16 << 20
const maxRecords = 1 << 18

type version struct {
	crc    uint32
	offset uint64
	width  int
}

// Module retains version entries and undefined ELF imports. Its internal byte
// locations are only used to construct a deliberately incompatible test module.
type Module struct {
	Basic    bool
	Extended bool
	versions map[string]version
	imports  map[string]bool // true means weak
	order    binary.ByteOrder
}

// CRC returns the loader-preferred CRC for name, including legitimate zero CRCs.
func (m *Module) CRC(name string) (uint32, bool) {
	v, ok := m.versions[name]
	return v.crc, ok
}

func validName(name string) bool {
	if name == "" || len(name) > 4096 {
		return false
	}
	for _, c := range []byte(name) {
		if c <= ' ' || c >= 127 {
			return false
		}
	}
	return true
}

func parseBasic(data []byte, width int, order binary.ByteOrder, offset uint64) (map[string]version, error) {
	if width != 4 && width != 8 || len(data)%64 != 0 || len(data)/64 > maxRecords {
		return nil, fmt.Errorf("malformed basic version record size")
	}
	out := map[string]version{}
	for i := 0; i < len(data); i += 64 {
		crc := uint64(order.Uint32(data[i : i+4]))
		if width == 8 {
			crc = order.Uint64(data[i : i+8])
		}
		if crc > 0xffffffff {
			return nil, fmt.Errorf("basic CRC has nonzero bits outside u32")
		}
		nameBytes := data[i+width : i+64]
		end := bytes.IndexByte(nameBytes, 0)
		if end < 0 || !validName(string(nameBytes[:end])) {
			return nil, fmt.Errorf("malformed basic version name")
		}
		for _, b := range nameBytes[end:] {
			if b != 0 {
				return nil, fmt.Errorf("nonzero basic version name padding")
			}
		}
		name := string(nameBytes[:end])
		if _, exists := out[name]; exists {
			return nil, fmt.Errorf("duplicate basic version %q", name)
		}
		out[name] = version{uint32(crc), offset + uint64(i), width}
	}
	return out, nil
}

func parseExtended(crcs, names []byte, order binary.ByteOrder, offset uint64) (map[string]version, error) {
	if len(crcs)%4 != 0 || len(crcs)/4 > maxRecords {
		return nil, fmt.Errorf("malformed extended CRC array")
	}
	out := map[string]version{}
	for i := 0; i < len(crcs); i += 4 {
		end := bytes.IndexByte(names, 0)
		if end < 0 || !validName(string(names[:end])) {
			return nil, fmt.Errorf("malformed or missing extended version name")
		}
		name := string(names[:end])
		if _, exists := out[name]; exists {
			return nil, fmt.Errorf("duplicate extended version %q", name)
		}
		out[name] = version{order.Uint32(crcs[i : i+4]), offset + uint64(i), 4}
		names = names[end+1:]
	}
	// modpost emits "first\0" "second\0"; C adds a final implicit terminator.
	if len(names) != 0 && (len(names) != 1 || names[0] != 0) {
		return nil, fmt.Errorf("extended name and CRC counts differ")
	}
	return out, nil
}

// ReadModule parses an uncompressed relocatable .ko image. Basic entries use a
// target unsigned long followed by 64-sizeof(unsigned long) name bytes; extended
// CRCs are target-endian u32 regardless of ELF class.
func ReadModule(image []byte) (*Module, error) {
	if len(image) > MaxImageBytes {
		return nil, fmt.Errorf("module exceeds size budget")
	}
	f, err := elf.NewFile(bytes.NewReader(image))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if f.Type != elf.ET_REL || f.ByteOrder == nil {
		return nil, fmt.Errorf("expected relocatable module ELF")
	}
	width := 4
	if f.Class == elf.ELFCLASS64 {
		width = 8
	} else if f.Class != elf.ELFCLASS32 {
		return nil, fmt.Errorf("unsupported ELF class")
	}
	sections := map[string]*elf.Section{}
	for _, s := range f.Sections {
		if s.Name != "__versions" && s.Name != "__version_ext_crcs" && s.Name != "__version_ext_names" && s.Type != elf.SHT_SYMTAB {
			continue
		}
		if sections[s.Name] != nil || s.Size > maxSectionBytes || s.Flags&elf.SHF_COMPRESSED != 0 ||
			s.Type == elf.SHT_NOBITS || s.Offset > uint64(len(image)) || s.Size > uint64(len(image))-s.Offset {
			return nil, fmt.Errorf("invalid or repeated module section %q", s.Name)
		}
		sections[s.Name] = s
	}
	data := func(s *elf.Section) []byte { return image[s.Offset : s.Offset+s.Size] }
	m := &Module{versions: map[string]version{}, imports: map[string]bool{}, order: f.ByteOrder}
	if s := sections["__versions"]; s != nil {
		m.Basic = true
		m.versions, err = parseBasic(data(s), width, f.ByteOrder, s.Offset)
		if err != nil {
			return nil, err
		}
	}
	crcs, names := sections["__version_ext_crcs"], sections["__version_ext_names"]
	if (crcs == nil) != (names == nil) {
		return nil, fmt.Errorf("incomplete extended version sections")
	}
	if crcs != nil {
		m.Extended = true
		extended, err := parseExtended(data(crcs), data(names), f.ByteOrder, crcs.Offset)
		if err != nil {
			return nil, err
		}
		for name, v := range extended {
			if old, exists := m.versions[name]; exists && old.crc != v.crc {
				return nil, fmt.Errorf("basic/extended CRC disagreement for %q", name)
			}
		}
		// With an extended table, the kernel does not fall back to basic for
		// names omitted from it. Missing effective records must fail validation.
		m.versions = extended
	}
	if len(m.versions) == 0 {
		return nil, fmt.Errorf("module has no version records")
	}
	symbols, err := f.Symbols()
	if err != nil {
		return nil, fmt.Errorf("module symbol table: %w", err)
	}
	if len(symbols) > maxRecords {
		return nil, fmt.Errorf("module symbol count exceeds budget")
	}
	for _, s := range symbols {
		bind := elf.ST_BIND(s.Info)
		if s.Section != elf.SHN_UNDEF || bind != elf.STB_GLOBAL && bind != elf.STB_WEAK {
			continue
		}
		// These are modpost's architecture-independent non-import exceptions.
		if s.Name == "__this_module" || s.Name == "_GLOBAL_OFFSET_TABLE_" {
			continue
		}
		if !validName(s.Name) {
			return nil, fmt.Errorf("invalid undefined ELF symbol")
		}
		weak := bind == elf.STB_WEAK
		if previous, exists := m.imports[s.Name]; exists {
			weak = weak && previous
		}
		m.imports[s.Name] = weak
	}
	return m, nil
}

// ParseSymvers reads upstream's five tab-separated fields. CRC zero is valid.
func ParseSymvers(r io.Reader) (map[string]uint32, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSectionBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSectionBytes {
		return nil, fmt.Errorf("symvers exceeds size budget")
	}
	out := map[string]uint32{}
	lines := strings.Split(string(data), "\n")
	for n, line := range lines {
		if line == "" && n == len(lines)-1 {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 5 || !validName(fields[1]) || !validName(fields[2]) ||
			(fields[3] != "EXPORT_SYMBOL" && fields[3] != "EXPORT_SYMBOL_GPL") || len(fields[0]) < 3 ||
			len(fields[0]) > 10 || !strings.HasPrefix(fields[0], "0x") {
			return nil, fmt.Errorf("malformed symvers line %d", n+1)
		}
		crc, err := strconv.ParseUint(fields[0][2:], 16, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed symvers CRC at line %d", n+1)
		}
		if _, exists := out[fields[1]]; exists {
			return nil, fmt.Errorf("duplicate symvers symbol %q", fields[1])
		}
		if len(out) >= maxRecords {
			return nil, fmt.Errorf("symvers symbol count exceeds budget")
		}
		out[fields[1]] = uint32(crc)
	}
	return out, nil
}

// MergeSymvers rejects ambiguous ownership even if duplicate CRCs happen to agree.
func MergeSymvers(tables ...map[string]uint32) (map[string]uint32, error) {
	out := map[string]uint32{}
	for _, table := range tables {
		for name, crc := range table {
			if _, exists := out[name]; exists {
				return nil, fmt.Errorf("multiple symvers owners for %q", name)
			}
			out[name] = crc
		}
	}
	return out, nil
}

// Validate requires module_layout and every resolved weak or strong import.
// Additional required names bind fixture dependency calls independently of the
// object's undefined-symbol table. Every recorded CRC must match its producer.
func Validate(m *Module, producers map[string]uint32, required ...string) error {
	if m == nil {
		return fmt.Errorf("nil module")
	}
	check := func(name string) error {
		want, exists := producers[name]
		if !exists {
			return fmt.Errorf("missing producer CRC for %q", name)
		}
		got, exists := m.versions[name]
		if !exists {
			return fmt.Errorf("missing module CRC for %q", name)
		}
		if got.crc != want {
			return fmt.Errorf("CRC mismatch for %q: module %#x producer %#x", name, got.crc, want)
		}
		return nil
	}
	for _, name := range append([]string{"module_layout"}, required...) {
		if err := check(name); err != nil {
			return err
		}
	}
	for name, weak := range m.imports {
		if _, exists := producers[name]; weak && !exists {
			continue
		}
		if err := check(name); err != nil {
			return err
		}
	}
	for name := range m.versions {
		if err := check(name); err != nil {
			return err
		}
	}
	return nil
}

// WithAlteredCRC clones image and toggles one low CRC bit in exactly the
// loader-preferred physical field. It never uses a sentinel CRC value.
func WithAlteredCRC(image []byte, symbol string) ([]byte, error) {
	if bytes.HasSuffix(image, []byte("~Module signature appended~\n")) {
		return nil, fmt.Errorf("cannot alter signed module: signature rejection would mask CRC rejection")
	}
	m, err := ReadModule(image)
	if err != nil {
		return nil, err
	}
	v, found := m.versions[symbol]
	if !found {
		return nil, fmt.Errorf("no version record for %q", symbol)
	}
	out := bytes.Clone(image)
	if v.width == 8 {
		m.order.PutUint64(out[v.offset:v.offset+8], uint64(v.crc^1))
	} else {
		m.order.PutUint32(out[v.offset:v.offset+4], v.crc^1)
	}
	return out, nil
}
