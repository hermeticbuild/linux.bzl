package main

import (
	"compress/gzip"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendSize(t *testing.T) {
	dir := t.TempDir()
	inA := filepath.Join(dir, "a")
	inB := filepath.Join(dir, "b")
	out := filepath.Join(dir, "out")
	if err := os.WriteFile(inA, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inB, []byte("de"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runAppendSize([]string{"-in", inA, "-in", inB, "-out", out}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[:5]) != "abcde" {
		t.Fatalf("payload = %q, want abcde", got[:5])
	}
	if size := binary.LittleEndian.Uint32(got[5:]); size != 5 {
		t.Fatalf("size = %d, want 5", size)
	}
}

func TestPiggy(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "kernel.lz4")
	out := filepath.Join(dir, "piggy.S")
	payload := append([]byte("compressed"), 1, 2, 3, 4)
	if err := os.WriteFile(in, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runPiggy([]string{"-in", in, "-out", out}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		`z_input_len = 14`,
		`z_output_len = 67305985`,
		`.incbin "` + in + `"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("piggy output missing %q:\n%s", want, text)
		}
	}
}

func TestGZIP(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "vmlinux.bin")
	outA := filepath.Join(dir, "vmlinux.bin.gz")
	outB := filepath.Join(dir, "vmlinux-again.bin.gz")
	want := []byte(strings.Repeat("kernel payload\n", 128))
	if err := os.WriteFile(in, want, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{outA, outB} {
		if err := runGZIP([]string{"-in", in, "-out", out}); err != nil {
			t.Fatal(err)
		}
	}
	gotA, err := os.ReadFile(outA)
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := os.ReadFile(outB)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotA) != string(gotB) {
		t.Fatal("gzip output is not deterministic")
	}
	reader, err := gzip.NewReader(strings.NewReader(string(gotA)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("decompressed payload differs: got %d bytes, want %d", len(got), len(want))
	}
}

func TestCPUString(t *testing.T) {
	dir := t.TempDir()
	cpufeatures := filepath.Join(dir, "cpufeatures.h")
	masks := filepath.Join(dir, "cpufeaturemasks.h")
	out := filepath.Join(dir, "cpustr.h")
	if err := os.WriteFile(cpufeatures, []byte(`
#define NCAPINTS 1
#define X86_FEATURE_FPU (0*32+0) /* "fpu" */
#define X86_FEATURE_ALWAYS (0*32+31) /* "always" */
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(masks, []byte(`#define REQUIRED_MASK0 0x00000001U
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCPUString([]string{"-cpufeatures", cpufeatures, "-masks", masks, "-out", out}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		"#if REQUIRED_MASK0 & (1 << 0)",
		`"\x00\x00""fpu\0"`,
		`"\x00\x1f""always"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("cpustr output missing %q:\n%s", want, text)
		}
	}
}
