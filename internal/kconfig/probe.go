// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kconfig

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const LinuxProbeModelLLVM = "linux_llvm"

var ifSuccessPattern = regexp.MustCompile(`^\{\s*(.*);\s*\}\s*>/dev/null\s+2>&1\s+&&\s+echo\s+"(.*)"\s+\|\|\s+echo\s+"(.*)"$`)

// LinuxProbeConfig describes deterministic answers for Linux Kconfig feature
// probes that upstream normally computes by executing compiler and host-tool
// commands during preprocessing.
type LinuxProbeConfig struct {
	CCName           string
	CCVersion        int
	ASName           string
	ASVersion        int
	LDName           string
	LDVersion        int
	RustcVersion     int
	RustcLLVMVersion int
	PaholeVersion    int
	BindgenVersion   string

	RustAvailable bool
	CanLink       bool
	CCOptions     bool
	ASInstr       bool
	RustOptions   bool
}

// LinuxProbeShell returns a deterministic shell model for Linux Kconfig feature
// probes. It is intentionally narrow: unsupported commands fail instead of
// silently inheriting host behavior.
func LinuxProbeShell(model string) (func(context.Context, string) (string, error), error) {
	config, err := LinuxProbeConfigForModel(model)
	if err != nil {
		return nil, err
	}
	return LinuxProbeShellFromConfig(config), nil
}

// LinuxProbeConfigForModel returns the default probe values for a named
// prototype model.
func LinuxProbeConfigForModel(model string) (LinuxProbeConfig, error) {
	switch model {
	case LinuxProbeModelLLVM:
		return LinuxProbeConfig{
			CCName:           "Clang",
			CCVersion:        220103,
			ASName:           "LLVM",
			ASVersion:        0,
			LDName:           "LLD",
			LDVersion:        220103,
			RustcVersion:     0,
			RustcLLVMVersion: 0,
			PaholeVersion:    0,
			CanLink:          true,
			CCOptions:        true,
			ASInstr:          true,
		}, nil
	default:
		return LinuxProbeConfig{}, fmt.Errorf("unknown Linux probe model %q", model)
	}
}

// LinuxProbeShellFromConfig returns a shell hook backed by explicit probe
// values. It is the boundary Bazel rules should eventually feed from toolchain
// providers.
func LinuxProbeShellFromConfig(config LinuxProbeConfig) func(context.Context, string) (string, error) {
	return (&linuxProbeShell{config: config}).run
}

// ApplyLinuxProbeValue applies a single string-keyed override. The key names
// are intentionally stable CLI/Bazel-facing API, while LinuxProbeConfig remains
// idiomatic Go.
func ApplyLinuxProbeValue(config *LinuxProbeConfig, key, value string) error {
	switch key {
	case "cc_name":
		config.CCName = value
	case "cc_version":
		return setProbeInt(&config.CCVersion, key, value)
	case "as_name":
		config.ASName = value
	case "as_version":
		return setProbeInt(&config.ASVersion, key, value)
	case "ld_name":
		config.LDName = value
	case "ld_version":
		return setProbeInt(&config.LDVersion, key, value)
	case "rustc_version":
		return setProbeInt(&config.RustcVersion, key, value)
	case "rustc_llvm_version":
		return setProbeInt(&config.RustcLLVMVersion, key, value)
	case "pahole_version":
		return setProbeInt(&config.PaholeVersion, key, value)
	case "bindgen_version":
		config.BindgenVersion = value
	case "rust_available":
		return setProbeBool(&config.RustAvailable, key, value)
	case "can_link":
		return setProbeBool(&config.CanLink, key, value)
	case "cc_options":
		return setProbeBool(&config.CCOptions, key, value)
	case "as_instr":
		return setProbeBool(&config.ASInstr, key, value)
	case "rust_options":
		return setProbeBool(&config.RustOptions, key, value)
	default:
		return fmt.Errorf("unknown Linux probe value %q", key)
	}
	return nil
}

type linuxProbeShell struct {
	config LinuxProbeConfig
}

func (s *linuxProbeShell) run(ctx context.Context, command string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	command = strings.TrimSpace(command)
	if match := ifSuccessPattern.FindStringSubmatch(command); match != nil {
		if s.commandSucceeds(match[1]) {
			return match[2], nil
		}
		return match[3], nil
	}
	return s.output(command)
}

func (s *linuxProbeShell) output(command string) (string, error) {
	switch {
	case strings.Contains(command, "/scripts/cc-version.sh"):
		return fmt.Sprintf("%s %d", s.config.CCName, s.config.CCVersion), nil
	case strings.Contains(command, "/scripts/as-version.sh"):
		return fmt.Sprintf("%s %d", s.config.ASName, s.config.ASVersion), nil
	case strings.Contains(command, "/scripts/ld-version.sh"):
		return fmt.Sprintf("%s %d", s.config.LDName, s.config.LDVersion), nil
	case strings.Contains(command, "/scripts/pahole-version.sh"):
		return strconv.Itoa(s.config.PaholeVersion), nil
	case strings.Contains(command, "/scripts/rustc-version.sh"):
		return strconv.Itoa(s.config.RustcVersion), nil
	case strings.Contains(command, "/scripts/rustc-llvm-version.sh"):
		return strconv.Itoa(s.config.RustcLLVMVersion), nil
	case strings.Contains(command, " --version") && strings.Contains(command, "bindgen"):
		return s.config.BindgenVersion, nil
	case strings.Contains(command, " -print-file-name=plugin"):
		return "", nil
	case strings.HasPrefix(command, "set -- "):
		return shellSetEcho(command)
	case strings.HasPrefix(command, "expr "):
		return shellExpr(command)
	default:
		return "", fmt.Errorf("unsupported Linux Kconfig probe command %q", command)
	}
}

func (s *linuxProbeShell) commandSucceeds(command string) bool {
	command = strings.TrimSpace(command)
	switch {
	case strings.HasPrefix(command, "command -v "):
		return strings.TrimSpace(strings.TrimPrefix(command, "command -v ")) != ""
	case strings.HasPrefix(command, "test "):
		return shellTest(strings.TrimSpace(strings.TrimPrefix(command, "test ")))
	case strings.Contains(command, "/scripts/rust_is_available.sh"):
		return s.config.RustAvailable
	case strings.Contains(command, "/scripts/cc-can-link.sh"):
		return s.config.CanLink
	case strings.Contains(command, " --crate-type=rlib "):
		return s.config.RustOptions
	case strings.Contains(command, " -Werror "):
		return s.config.CCOptions
	case strings.Contains(command, " -Wa,"):
		return s.config.ASInstr
	case strings.Contains(command, " -v "):
		return true
	default:
		return false
	}
}

func setProbeInt(dst *int, key, value string) error {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("invalid Linux probe value %s=%q: %w", key, value, err)
	}
	*dst = parsed
	return nil
}

func setProbeBool(dst *bool, key, value string) error {
	switch strings.ToLower(value) {
	case "1", "t", "true", "y", "yes":
		*dst = true
		return nil
	case "0", "f", "false", "n", "no":
		*dst = false
		return nil
	default:
		return fmt.Errorf("invalid Linux probe value %s=%q: expected boolean", key, value)
	}
}

func shellSetEcho(command string) (string, error) {
	before, after, ok := strings.Cut(command, "&&")
	if !ok {
		return "", fmt.Errorf("unsupported set command %q", command)
	}
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(before, "set -- ")))
	echo := strings.TrimSpace(after)
	if !strings.HasPrefix(echo, "echo $") {
		return "", fmt.Errorf("unsupported set echo command %q", command)
	}
	index, err := strconv.Atoi(strings.TrimPrefix(echo, "echo $"))
	if err != nil {
		return "", fmt.Errorf("unsupported set echo command %q", command)
	}
	if index < 1 || index > len(fields) {
		return "", nil
	}
	return fields[index-1], nil
}

func shellExpr(command string) (string, error) {
	fields := strings.Fields(command)
	if len(fields) == 4 && fields[2] == "/" {
		left, leftErr := strconv.Atoi(fields[1])
		right, rightErr := strconv.Atoi(fields[3])
		if leftErr != nil || rightErr != nil || right == 0 {
			return "", fmt.Errorf("unsupported expr command %q", command)
		}
		return strconv.Itoa(left / right), nil
	}
	return "", fmt.Errorf("unsupported expr command %q", command)
}

func shellTest(expr string) bool {
	if value, ok := strings.CutPrefix(expr, "-z "); ok {
		return unquoteShell(value) == ""
	}
	fields := strings.Fields(expr)
	if len(fields) != 3 {
		return false
	}
	left := unquoteShell(fields[0])
	right := unquoteShell(fields[2])
	switch fields[1] {
	case "=":
		return left == right
	case "!=":
		return left != right
	default:
		return false
	}
}

func unquoteShell(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}
