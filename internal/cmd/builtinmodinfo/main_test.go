package main

import (
	"bytes"
	"testing"
)

func TestProcessModinfo(t *testing.T) {
	raw := []byte(
		"foo.license=GPL\x00" +
			"foo.file=drivers/foo/foo\x00" +
			"shared.file=drivers/base/shared drivers/extra/shared\x00" +
			"bad-name.file=drivers/ignored\x00" +
			"foo.file=drivers/foo/foo\x00\x00\x00",
	)
	gotModinfo, gotModules := processModinfo(raw)
	wantModinfo := bytes.TrimRight(raw, "\x00")
	wantModinfo = append(wantModinfo, 0)
	if !bytes.Equal(gotModinfo, wantModinfo) {
		t.Fatalf("normalized modinfo = %q, want %q", gotModinfo, wantModinfo)
	}
	wantModules := "" +
		"kernel/drivers/foo/foo.ko\n" +
		"kernel/drivers/base/shared.ko\n" +
		"kernel/drivers/extra/shared.ko\n"
	if gotModules != wantModules {
		t.Fatalf("modules.builtin = %q, want %q", gotModules, wantModules)
	}
}

func TestProcessEmptyModinfo(t *testing.T) {
	gotModinfo, gotModules := processModinfo(nil)
	if len(gotModinfo) != 0 || gotModules != "" {
		t.Fatalf("processModinfo(nil) = %q, %q", gotModinfo, gotModules)
	}
}
