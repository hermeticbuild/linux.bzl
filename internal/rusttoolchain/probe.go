package rusttoolchain

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const ProbeSchema = "linux-rust-toolchain-probe-v1"

// Probe is the canonical identity of the rustc selected by Bazel toolchain
// resolution. VersionCode fields use Linux's major*100000+minor*100+patch
// encoding.
type Probe struct {
	Schema          string `json:"schema"`
	VersionText     string `json:"version_text"`
	Release         string `json:"release"`
	Semver          string `json:"semver"`
	Channel         string `json:"channel"`
	CommitDate      string `json:"commit_date,omitempty"`
	LLVMVersion     string `json:"llvm_version"`
	VersionCode     int    `json:"version_code"`
	LLVMVersionCode int    `json:"llvm_version_code"`
}

// ParseVerbose parses rustc -vV output.
func ParseVerbose(output string) (Probe, error) {
	values := map[string]string{}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "rustc ") {
		return Probe{}, fmt.Errorf("invalid rustc -vV output: missing rustc version line")
	}
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	release := values["release"]
	if release == "" {
		release = strings.TrimSpace(strings.TrimPrefix(lines[0], "rustc "))
		if index := strings.IndexByte(release, ' '); index >= 0 {
			release = release[:index]
		}
	}
	semver, channel, versionCode, err := ParseRelease(release)
	if err != nil {
		return Probe{}, err
	}
	llvmVersion := values["LLVM version"]
	llvmVersionCode, err := ParseLLVMVersionCode(llvmVersion)
	if err != nil {
		return Probe{}, fmt.Errorf("invalid rustc LLVM version %q: %w", llvmVersion, err)
	}
	return Probe{
		Schema:          ProbeSchema,
		VersionText:     lines[0],
		Release:         release,
		Semver:          semver,
		Channel:         channel,
		CommitDate:      values["commit-date"],
		LLVMVersion:     llvmVersion,
		VersionCode:     versionCode,
		LLVMVersionCode: llvmVersionCode,
	}, nil
}

// ParseRelease returns the stable semver prefix, channel, and Linux version
// code for a rustc release such as 1.98.0-nightly.
func ParseRelease(release string) (string, string, int, error) {
	semver, suffix, _ := strings.Cut(release, "-")
	channel := "stable"
	if suffix != "" {
		channel = strings.SplitN(suffix, ".", 2)[0]
	}
	code, err := ParseVersionCode(semver)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid rustc release %q: %w", release, err)
	}
	return semver, channel, code, nil
}

func ParseVersionCode(version string) (int, error) {
	return parseVersionCode(version, 100000, 999)
}

func ParseLLVMVersionCode(version string) (int, error) {
	return parseVersionCode(version, 10000, 99)
}

func parseVersionCode(version string, majorScale, maximumMinor int) (int, error) {
	parts := strings.Split(version, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("expected MAJOR.MINOR[.PATCH]")
	}
	numbers := []int{0, 0, 0}
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("invalid numeric component %q", part)
		}
		numbers[i] = value
	}
	if numbers[1] > maximumMinor || numbers[2] > 99 {
		return 0, fmt.Errorf("version component exceeds Linux encoding")
	}
	return numbers[0]*majorScale + numbers[1]*100 + numbers[2], nil
}

func (p Probe) Validate() error {
	if p.Schema != ProbeSchema {
		return fmt.Errorf("unsupported Rust toolchain probe schema %q", p.Schema)
	}
	versionFields := strings.Fields(p.VersionText)
	if len(versionFields) < 2 || versionFields[0] != "rustc" || versionFields[1] != p.Release {
		return fmt.Errorf("invalid rustc version text %q for release %q", p.VersionText, p.Release)
	}
	semver, channel, code, err := ParseRelease(p.Release)
	if err != nil {
		return err
	}
	llvmCode, err := ParseLLVMVersionCode(p.LLVMVersion)
	if err != nil {
		return fmt.Errorf("invalid LLVM version: %w", err)
	}
	if p.Semver != semver || p.Channel != channel || p.VersionCode != code || p.LLVMVersionCode != llvmCode {
		return fmt.Errorf("inconsistent Rust toolchain probe")
	}
	return nil
}

func (p Probe) AtLeast(version string) (bool, error) {
	minimum, err := ParseVersionCode(version)
	if err != nil {
		return false, err
	}
	return p.VersionCode >= minimum, nil
}

func Decode(r io.Reader) (Probe, error) {
	var probe Probe
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&probe); err != nil {
		return Probe{}, err
	}
	if err := probe.Validate(); err != nil {
		return Probe{}, err
	}
	return probe, nil
}

func Encode(w io.Writer, probe Probe) error {
	if err := probe.Validate(); err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(probe)
}
