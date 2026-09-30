package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/hermeticbuild/linux.bzl/e2e/internal/modversions"
)

const (
	bootMarker        = "LINUX_BZL_BOOT_OK"
	moduleLoadMarker  = "LINUX_BZL_MODULE_LOAD_OK"
	modversionsMarker = "LINUX_BZL_MODVERSIONS_OK"
	testModulePath    = "/test_module.ko"
)

type moduleLoader func(fd uintptr) error

func boot(modulePath string, load moduleLoader, out io.Writer) error {
	module, err := os.Open(modulePath)
	if errors.Is(err, os.ErrNotExist) {
		_, err = fmt.Fprintln(out, bootMarker)
		return err
	}
	if err != nil {
		return fmt.Errorf("open test module: %w", err)
	}
	defer module.Close()

	if err := load(module.Fd()); err != nil {
		return fmt.Errorf("load test module: %w", err)
	}
	_, err = fmt.Fprintln(out, moduleLoadMarker)
	return err
}

// bootVersioned proves that the running kernel enforces module CRCs, then loads
// the untouched module. The mutation changes only module_layout's CRC, not the
// ELF structure, vermagic, or code (signed modules cannot be mutated). A
// build-only test cannot detect version records silently dropped by modpost.
func bootVersioned(modulePath string, alter func([]byte) ([]byte, error), load moduleLoader, out io.Writer) error {
	original, err := os.ReadFile(modulePath)
	if err != nil {
		return fmt.Errorf("read versioned module: %w", err)
	}
	changed, err := alter(bytes.Clone(original))
	if err != nil {
		return fmt.Errorf("alter module CRC: %w", err)
	}
	if len(changed) != len(original) || bytes.Equal(changed, original) {
		return errors.New("CRC mutation must change the module without changing its size")
	}
	bad, err := os.CreateTemp(filepath.Dir(modulePath), "bad-crc-*.ko")
	if err != nil {
		return fmt.Errorf("create CRC-altered module: %w", err)
	}
	defer os.Remove(bad.Name())
	_, writeErr := bad.Write(changed)
	closeErr := bad.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write CRC-altered module: %w", err)
	}
	// Reopen read-only: finit_module rejects a file still open for writing,
	// which would otherwise make the negative check pass for the wrong reason.
	bad, err = os.Open(bad.Name())
	if err != nil {
		return fmt.Errorf("open CRC-altered module: %w", err)
	}
	defer bad.Close()
	if err := load(bad.Fd()); err == nil {
		return errors.New("kernel accepted a module with an incorrect module_layout CRC")
	} else if !errors.Is(err, syscall.ENOEXEC) {
		return fmt.Errorf("CRC-altered module rejected for an unexpected reason (want ENOEXEC): %w", err)
	}
	module, err := os.Open(modulePath)
	if err != nil {
		return fmt.Errorf("open original versioned module: %w", err)
	}
	defer module.Close()
	if err := load(module.Fd()); err != nil {
		return fmt.Errorf("load original versioned module: %w", err)
	}
	_, err = fmt.Fprintln(out, modversionsMarker)
	return err
}

func finitModuleSyscall(arch string) (uintptr, error) {
	switch arch {
	case "amd64":
		return 313, nil
	case "arm64":
		return 273, nil
	default:
		return 0, fmt.Errorf("unsupported architecture %q", arch)
	}
}

func finitModule(fd uintptr) error {
	trap, err := finitModuleSyscall(runtime.GOARCH)
	if err != nil {
		return err
	}
	params := []byte{0}
	_, _, errno := syscall.Syscall(
		trap,
		fd,
		uintptr(unsafe.Pointer(&params[0])),
		0,
	)
	runtime.KeepAlive(params)
	if errno != 0 {
		return errno
	}
	return nil
}

func main() {
	checkModversions := flag.Bool("check-modversions", false, "verify that the kernel rejects a changed module CRC")
	flag.Parse()
	var err error
	if *checkModversions {
		err = bootVersioned(testModulePath, func(image []byte) ([]byte, error) {
			return modversions.WithAlteredCRC(image, "module_layout")
		}, finitModule, os.Stdout)
	} else {
		err = boot(testModulePath, finitModule, os.Stdout)
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "LINUX_BZL_BOOT_ERROR: %v\n", err)
	}
	select {}
}
