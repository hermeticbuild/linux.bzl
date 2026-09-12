package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestBootWithoutModule(t *testing.T) {
	var out bytes.Buffer
	called := false
	err := boot(
		filepath.Join(t.TempDir(), "missing.ko"),
		func(uintptr) error {
			called = true
			return nil
		},
		&out,
	)
	if err != nil {
		t.Fatalf("boot() error = %v", err)
	}
	if called {
		t.Fatal("module loader called for missing optional module")
	}
	if got, want := out.String(), bootMarker+"\n"; got != want {
		t.Fatalf("boot() output = %q, want %q", got, want)
	}
}

func TestBootLoadsModule(t *testing.T) {
	modulePath := filepath.Join(t.TempDir(), "test_module.ko")
	if err := os.WriteFile(modulePath, []byte("module"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	called := false
	err := boot(
		modulePath,
		func(fd uintptr) error {
			called = true
			var stat syscall.Stat_t
			if err := syscall.Fstat(int(fd), &stat); err != nil {
				t.Fatalf("module fd is not open: %v", err)
			}
			return nil
		},
		&out,
	)
	if err != nil {
		t.Fatalf("boot() error = %v", err)
	}
	if !called {
		t.Fatal("module loader was not called")
	}
	if got, want := out.String(), moduleLoadMarker+"\n"; got != want {
		t.Fatalf("boot() output = %q, want %q", got, want)
	}
}

func TestBootReportsModuleLoadFailure(t *testing.T) {
	modulePath := filepath.Join(t.TempDir(), "test_module.ko")
	if err := os.WriteFile(modulePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := boot(modulePath, func(uintptr) error { return syscall.EINVAL }, &out)
	if !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("boot() error = %v, want EINVAL", err)
	}
	if out.Len() != 0 {
		t.Fatalf("boot() output = %q, want no success marker", out.String())
	}
}

func TestFinitModuleSyscall(t *testing.T) {
	tests := []struct {
		arch string
		want uintptr
	}{
		{arch: "amd64", want: 313},
		{arch: "arm64", want: 273},
	}
	for _, test := range tests {
		t.Run(test.arch, func(t *testing.T) {
			got, err := finitModuleSyscall(test.arch)
			if err != nil {
				t.Fatalf("finitModuleSyscall() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("finitModuleSyscall() = %d, want %d", got, test.want)
			}
		})
	}

	_, err := finitModuleSyscall("unsupported")
	if err == nil || !strings.Contains(err.Error(), "unsupported architecture") {
		t.Fatalf("finitModuleSyscall() error = %v, want unsupported architecture", err)
	}
}

func TestBootVersioned(t *testing.T) {
	for _, tc := range []struct {
		name      string
		badErr    error
		goodErr   error
		wantErr   string
		wantLoads int
	}{
		{name: "reject changed CRC and accept original", badErr: syscall.ENOEXEC, wantLoads: 2},
		{name: "accept changed CRC", wantErr: "kernel accepted", wantLoads: 1},
		{name: "reject original", badErr: syscall.ENOEXEC, goodErr: syscall.EINVAL, wantErr: "load original", wantLoads: 2},
		{name: "permission failure is not CRC enforcement", badErr: syscall.EPERM, wantErr: "unexpected reason", wantLoads: 1},
		{name: "memory failure is not CRC enforcement", badErr: syscall.ENOMEM, wantErr: "unexpected reason", wantLoads: 1},
		{name: "writable file is not CRC enforcement", badErr: syscall.ETXTBSY, wantErr: "unexpected reason", wantLoads: 1},
		{name: "signature rejection is not CRC enforcement", badErr: syscall.EKEYREJECTED, wantErr: "unexpected reason", wantLoads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "module.ko")
			original := []byte("original module")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			alter := func(image []byte) ([]byte, error) {
				image[0] ^= 1
				return image, nil
			}
			loads := 0
			load := func(fd uintptr) error {
				loads++
				got := make([]byte, len(original))
				n, err := syscall.Pread(int(fd), got, 0)
				if err != nil || n != len(got) {
					t.Fatalf("read module fd: n=%d, err=%v", n, err)
				}
				// A writable file would make finit_module fail before CRC validation.
				if _, err := syscall.Pwrite(int(fd), got, 0); err != syscall.EBADF {
					t.Fatalf("module fd should be read-only, write error=%v", err)
				}
				want := bytes.Clone(original)
				if loads == 1 {
					want[0] ^= 1
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("load %d: bytes=%q, want %q", loads, got, want)
				}
				if loads == 1 {
					return tc.badErr
				}
				return tc.goodErr
			}
			var out bytes.Buffer
			err := bootVersioned(path, alter, load, &out)
			if tc.wantErr == "" {
				if err != nil || out.String() != modversionsMarker+"\n" {
					t.Fatalf("bootVersioned() err=%v output=%q", err, out.String())
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) || out.Len() != 0 {
				t.Fatalf("bootVersioned() err=%v output=%q, want %q and no success marker", err, out.String(), tc.wantErr)
			}
			if loads != tc.wantLoads {
				t.Fatalf("loads=%d, want %d", loads, tc.wantLoads)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("original module changed: bytes=%q err=%v", got, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "module.ko" {
				t.Fatalf("temporary module not cleaned up: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestBootVersionedFailsBeforeLoad(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing bool
		alter   func([]byte) ([]byte, error)
	}{
		{name: "missing module", missing: true},
		{name: "mutation error", alter: func([]byte) ([]byte, error) { return nil, errors.New("missing CRC") }},
		{name: "unchanged", alter: func(b []byte) ([]byte, error) { return b, nil }},
		{name: "wrong size", alter: func([]byte) ([]byte, error) { return nil, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "module.ko")
			if !tc.missing {
				if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			err := bootVersioned(path, tc.alter, func(uintptr) error {
				t.Fatal("loader called before successful CRC mutation")
				return nil
			}, &out)
			if err == nil || out.Len() != 0 {
				t.Fatalf("bootVersioned() err=%v output=%q, want error and no success marker", err, out.String())
			}
		})
	}
}
