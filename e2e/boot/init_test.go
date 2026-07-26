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
