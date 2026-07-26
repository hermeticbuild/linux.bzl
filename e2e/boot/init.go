package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	bootMarker       = "LINUX_BZL_BOOT_OK"
	moduleLoadMarker = "LINUX_BZL_MODULE_LOAD_OK"
	testModulePath   = "/test_module.ko"
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
	if err := boot(testModulePath, finitModule, os.Stdout); err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "LINUX_BZL_BOOT_ERROR: %v\n", err)
	}
	select {}
}
