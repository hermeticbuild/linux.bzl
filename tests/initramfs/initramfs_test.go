package initramfs_test

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

const (
	newcHeaderSize = 110
	newcMagic      = "070701"
)

type newcEntry struct {
	name      string
	inode     uint32
	mode      uint32
	uid       uint32
	gid       uint32
	nlink     uint32
	mtime     uint32
	devMajor  uint32
	devMinor  uint32
	rdevMajor uint32
	rdevMinor uint32
	check     uint32
	body      []byte
}

var expectedEntries = []newcEntry{
	{name: "bin", inode: 1, mode: 0o040755, nlink: 2},
	{name: "dev", inode: 2, mode: 0o040755, nlink: 2},
	{name: "empty", inode: 3, mode: 0o040755, nlink: 2},
	{name: "empty/dir", inode: 4, mode: 0o040755, nlink: 2},
	{name: "etc", inode: 5, mode: 0o040755, nlink: 2},
	{name: "usr", inode: 6, mode: 0o040755, nlink: 2},
	{name: "usr/bin", inode: 7, mode: 0o040755, nlink: 2},
	{name: "etc/config", inode: 8, mode: 0o100644, nlink: 1, body: []byte("config\n")},
	{name: "bin/tool", inode: 9, mode: 0o100755, nlink: 1, body: []byte("tool\n")},
	{name: "init", inode: 10, mode: 0o100755, nlink: 1, body: []byte("tool\n")},
	{name: "usr/bin/tool", inode: 11, mode: 0o100755, nlink: 1, body: []byte("tool\n")},
	{name: "bin/config", inode: 12, mode: 0o120777, nlink: 1, body: []byte("/etc/config")},
	{name: "dev/null", inode: 13, mode: 0o020666, nlink: 1, rdevMajor: 1, rdevMinor: 3},
	{name: "TRAILER!!!", inode: 14, nlink: 1},
}

func TestInitramfs(t *testing.T) {
	paths := flag.Args()
	if len(paths) == 0 {
		t.Skip("archive inputs are supplied by the Bazel target")
	}
	if len(paths) != 2 {
		t.Fatalf("got %d archive arguments, want 2", len(paths))
	}

	archive := readArchive(t, paths[0])
	reordered := readArchive(t, paths[1])
	if !bytes.Equal(archive, reordered) {
		t.Fatal("reordered inputs produced a different archive")
	}
	if len(archive)%512 != 0 {
		t.Fatalf("archive size %d is not aligned to 512 bytes", len(archive))
	}

	entries, offset, err := parseNewc(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(expectedEntries) {
		t.Fatalf("got %d entries, want %d", len(entries), len(expectedEntries))
	}
	for i := range expectedEntries {
		if err := compareEntry(entries[i], expectedEntries[i]); err != nil {
			t.Errorf("entry %d: %v", i, err)
		}
	}
	if padding := archive[offset:]; !allZero(padding) {
		t.Fatalf("final %d-byte padding contains nonzero data", len(padding))
	}
}

func readArchive(t *testing.T, path string) []byte {
	t.Helper()
	resolved, err := runfiles.Rlocation(path)
	if err != nil {
		t.Fatalf("resolve %q: %v", path, err)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return data
}

func parseNewc(archive []byte) ([]newcEntry, int, error) {
	var entries []newcEntry
	offset := 0
	for {
		if len(archive)-offset < newcHeaderSize {
			return nil, 0, fmt.Errorf("truncated header at offset %d", offset)
		}
		header := archive[offset : offset+newcHeaderSize]
		if string(header[:6]) != newcMagic {
			return nil, 0, fmt.Errorf("invalid magic %q at offset %d", header[:6], offset)
		}
		offset += newcHeaderSize

		fields := make([]uint32, 13)
		for i := range fields {
			value, err := strconv.ParseUint(string(header[6+i*8:14+i*8]), 16, 32)
			if err != nil {
				return nil, 0, fmt.Errorf("field %d at offset %d: %w", i, offset-newcHeaderSize, err)
			}
			fields[i] = uint32(value)
		}

		nameSize := int(fields[11])
		if nameSize < 1 || nameSize > len(archive)-offset {
			return nil, 0, fmt.Errorf("invalid name size %d at offset %d", nameSize, offset)
		}
		nameBytes := archive[offset : offset+nameSize]
		if nameBytes[nameSize-1] != 0 || bytes.IndexByte(nameBytes[:nameSize-1], 0) >= 0 {
			return nil, 0, fmt.Errorf("invalid NUL-terminated name at offset %d", offset)
		}
		offset += nameSize
		var err error
		offset, err = skipZeroPadding(archive, offset, 4)
		if err != nil {
			return nil, 0, err
		}

		bodySize := int(fields[6])
		if bodySize > len(archive)-offset {
			return nil, 0, fmt.Errorf("truncated body at offset %d: need %d bytes", offset, bodySize)
		}
		body := bytes.Clone(archive[offset : offset+bodySize])
		offset += bodySize
		offset, err = skipZeroPadding(archive, offset, 4)
		if err != nil {
			return nil, 0, err
		}

		entry := newcEntry{
			name:      string(nameBytes[:nameSize-1]),
			inode:     fields[0],
			mode:      fields[1],
			uid:       fields[2],
			gid:       fields[3],
			nlink:     fields[4],
			mtime:     fields[5],
			devMajor:  fields[7],
			devMinor:  fields[8],
			rdevMajor: fields[9],
			rdevMinor: fields[10],
			check:     fields[12],
			body:      body,
		}
		entries = append(entries, entry)
		if entry.name == "TRAILER!!!" {
			return entries, offset, nil
		}
	}
}

func skipZeroPadding(data []byte, offset, alignment int) (int, error) {
	padding := (alignment - offset%alignment) % alignment
	if padding > len(data)-offset {
		return 0, fmt.Errorf("truncated padding at offset %d", offset)
	}
	if !allZero(data[offset : offset+padding]) {
		return 0, fmt.Errorf("nonzero padding at offset %d", offset)
	}
	return offset + padding, nil
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func compareEntry(got, want newcEntry) error {
	if got.name != want.name {
		return fmt.Errorf("name = %q, want %q", got.name, want.name)
	}
	if got.inode != want.inode {
		return fmt.Errorf("%s: inode = %d, want %d", got.name, got.inode, want.inode)
	}
	if got.mode != want.mode || got.uid != want.uid || got.gid != want.gid ||
		got.nlink != want.nlink || got.mtime != want.mtime ||
		got.devMajor != want.devMajor || got.devMinor != want.devMinor ||
		got.rdevMajor != want.rdevMajor || got.rdevMinor != want.rdevMinor ||
		got.check != want.check {
		return fmt.Errorf(
			"%s: metadata = (mode=%#o uid=%d gid=%d nlink=%d mtime=%d dev=%d:%d rdev=%d:%d check=%d), want (%#o,%d,%d,%d,%d,%d:%d,%d:%d,%d)",
			got.name,
			got.mode, got.uid, got.gid, got.nlink, got.mtime, got.devMajor, got.devMinor, got.rdevMajor, got.rdevMinor, got.check,
			want.mode, want.uid, want.gid, want.nlink, want.mtime, want.devMajor, want.devMinor, want.rdevMajor, want.rdevMinor, want.check,
		)
	}
	if !bytes.Equal(got.body, want.body) {
		return fmt.Errorf("%s: body = %q, want %q", got.name, got.body, want.body)
	}
	return nil
}
