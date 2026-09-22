package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

// Validate the staged assignment file before executing any repeated source
// prelude. A planner's snapshot cannot prove that a later selected Kbuild
// writer did not replace that file with shell commands.
func validateStagedStaticSourceAssignments(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("staged static source assignments require a canonical absolute path")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open staged static source assignments: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect staged static source assignments: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxScriptBytes {
		return fmt.Errorf("staged static source assignments must be a regular file of at most %d bytes", maxScriptBytes)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxScriptBytes+1))
	if err != nil {
		return fmt.Errorf("read staged static source assignments: %w", err)
	}
	if len(contents) > maxScriptBytes {
		return fmt.Errorf("staged static source assignments exceed %d bytes", maxScriptBytes)
	}
	if err := toolaction.ValidateStaticConfigAssignments(string(contents)); err != nil {
		return fmt.Errorf("staged static source assignments: %w", err)
	}
	return nil
}
