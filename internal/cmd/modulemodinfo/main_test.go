package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckModuleModinfoRejectsSourceVersions(t *testing.T) {
	for _, entry := range []string{
		"version=1.0",
		"srcversion=0123456789ABCDEF",
	} {
		t.Run(entry, func(t *testing.T) {
			err := checkModuleModinfo([]byte("license=GPL\x00" + entry + "\x00"))
			if err == nil || !strings.Contains(err.Error(), "source-version metadata") {
				t.Fatalf("checkModuleModinfo() error = %v, want source-version rejection", err)
			}
		})
	}
}

func TestCheckModuleModinfoAllowsOtherMetadata(t *testing.T) {
	data := []byte("license=GPL\x00description=example\x00myversion=1.0\x00")
	if err := checkModuleModinfo(data); err != nil {
		t.Fatal(err)
	}
}

func TestRunAcceptsELFWithoutModinfo(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "checked")
	if err := run(executable, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "ok\n"; got != want {
		t.Fatalf("validation marker = %q, want %q", got, want)
	}
}

func TestRunRejectsELFWithVersionModinfo(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "module.o")
	writeELFWithModinfo(t, in, []byte("license=GPL\x00version=1.0\x00"))

	err := run(in, filepath.Join(dir, "checked"))
	if err == nil || !strings.Contains(err.Error(), "source-version metadata") {
		t.Fatalf("run() error = %v, want source-version rejection", err)
	}
}

func TestRunRejectsNonELF(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "input")
	if err := os.WriteFile(in, []byte("not an ELF"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(in, filepath.Join(dir, "checked"))
	if err == nil || !strings.Contains(err.Error(), "open module ELF") {
		t.Fatalf("run() error = %v, want invalid ELF rejection", err)
	}
}

func writeELFWithModinfo(t *testing.T, path string, modinfo []byte) {
	t.Helper()

	const (
		headerSize        = 64
		sectionHeaderSize = 64
		sectionCount      = 3
	)
	shstrtab := []byte("\x00.shstrtab\x00.modinfo\x00")
	dataOffset := headerSize + sectionHeaderSize*sectionCount
	data := make([]byte, dataOffset+len(shstrtab)+len(modinfo))

	copy(data, "\x7fELF")
	data[4] = 2                                  // ELFCLASS64
	data[5] = 1                                  // ELFDATA2LSB
	data[6] = 1                                  // EV_CURRENT
	binary.LittleEndian.PutUint16(data[16:], 1)  // ET_REL
	binary.LittleEndian.PutUint16(data[18:], 62) // EM_X86_64
	binary.LittleEndian.PutUint32(data[20:], 1)  // EV_CURRENT
	binary.LittleEndian.PutUint64(data[40:], headerSize)
	binary.LittleEndian.PutUint16(data[52:], headerSize)
	binary.LittleEndian.PutUint16(data[58:], sectionHeaderSize)
	binary.LittleEndian.PutUint16(data[60:], sectionCount)
	binary.LittleEndian.PutUint16(data[62:], 1)

	shstrtabHeader := headerSize + sectionHeaderSize
	binary.LittleEndian.PutUint32(data[shstrtabHeader:], 1)
	binary.LittleEndian.PutUint32(data[shstrtabHeader+4:], 3) // SHT_STRTAB
	binary.LittleEndian.PutUint64(data[shstrtabHeader+24:], uint64(dataOffset))
	binary.LittleEndian.PutUint64(data[shstrtabHeader+32:], uint64(len(shstrtab)))
	binary.LittleEndian.PutUint64(data[shstrtabHeader+48:], 1)

	modinfoHeader := headerSize + sectionHeaderSize*2
	binary.LittleEndian.PutUint32(data[modinfoHeader:], 11)
	binary.LittleEndian.PutUint32(data[modinfoHeader+4:], 1) // SHT_PROGBITS
	binary.LittleEndian.PutUint64(data[modinfoHeader+24:], uint64(dataOffset+len(shstrtab)))
	binary.LittleEndian.PutUint64(data[modinfoHeader+32:], uint64(len(modinfo)))
	binary.LittleEndian.PutUint64(data[modinfoHeader+48:], 1)

	copy(data[dataOffset:], shstrtab)
	copy(data[dataOffset+len(shstrtab):], modinfo)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
