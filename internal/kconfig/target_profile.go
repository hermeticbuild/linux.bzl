package kconfig

import (
	"fmt"
	"sort"
	"strings"
)

// LinuxTargetProfile is the immutable architecture information selected by the
// Bazel target platform. Kconfig fragments are policy inputs, not an
// architecture selection mechanism.
type LinuxTargetProfile struct {
	Name         string
	Arch         string
	Srcarch      string
	UTSMachine   string
	TargetTriple string
	ABISeeds     map[string]string
}

var linuxTargetProfiles = map[string]LinuxTargetProfile{
	"x86_64": {
		Name: "x86_64", Arch: "x86", Srcarch: "x86", UTSMachine: "x86_64",
		TargetTriple: "x86_64-linux-gnu", ABISeeds: map[string]string{"CONFIG_64BIT": "y"},
	},
	"aarch64": {
		Name: "aarch64", Arch: "arm64", Srcarch: "arm64", UTSMachine: "aarch64",
		TargetTriple: "aarch64-linux-gnu", ABISeeds: map[string]string{},
	},
	"armv7": {
		Name: "armv7", Arch: "arm", Srcarch: "arm", UTSMachine: "armv7l",
		TargetTriple: "arm-linux-gnueabi", ABISeeds: map[string]string{},
	},
}

// LinuxTargetProfileByName returns a copy of the canonical profile.
func LinuxTargetProfileByName(name string) (LinuxTargetProfile, error) {
	profile, ok := linuxTargetProfiles[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		names := make([]string, 0, len(linuxTargetProfiles))
		for candidate := range linuxTargetProfiles {
			names = append(names, candidate)
		}
		sort.Strings(names)
		return LinuxTargetProfile{}, fmt.Errorf("unsupported Linux target profile %q; expected one of %s", name, strings.Join(names, ", "))
	}
	profile.ABISeeds = cloneStringMap(profile.ABISeeds)
	return profile, nil
}

// ValidateTargetIdentity rejects a caller that combines independently chosen
// architecture values. All three values must describe the named profile.
func (p LinuxTargetProfile) ValidateTargetIdentity(arch, triple string) error {
	if arch != "" && arch != p.Arch {
		return fmt.Errorf("Linux target profile %q requires ARCH=%q, got %q", p.Name, p.Arch, arch)
	}
	if triple != "" && triple != p.TargetTriple {
		return fmt.Errorf("Linux target profile %q requires target triple %q, got %q", p.Name, p.TargetTriple, triple)
	}
	return nil
}

var architectureSelectorOwners = map[string]string{
	"CONFIG_X86":    "x86_64",
	"CONFIG_X86_64": "x86_64",
	"CONFIG_ARM64":  "aarch64",
	"CONFIG_ARM":    "armv7",
	// x86_32 is intentionally unsupported. Owning its selector separately
	// makes CONFIG_X86_32=y fail closed for every public target profile.
	"CONFIG_X86_32": "x86_32",
}

var requiredArchitectureSelectors = map[string][]string{
	"x86_64":  {"CONFIG_X86", "CONFIG_X86_64"},
	"aarch64": {"CONFIG_ARM64"},
	"armv7":   {"CONFIG_ARM"},
}

// PrepareTargetConfig validates architecture/ABI selectors supplied by a user
// fragment and adds only immutable ABI seeds. Root architecture symbols such as
// CONFIG_ARM are deliberately not seeded: the architecture Kconfig selected by
// ARCH derives them.
func (p LinuxTargetProfile) PrepareTargetConfig(raw map[string]string) (map[string]string, error) {
	prepared := cloneStringMap(raw)
	for symbol, owner := range architectureSelectorOwners {
		if prepared[symbol] == "y" && owner != p.Name {
			return nil, fmt.Errorf("%s=y selects Linux target profile %q, but the target platform selects %q", symbol, owner, p.Name)
		}
	}
	for _, symbol := range requiredArchitectureSelectors[p.Name] {
		if configured, ok := prepared[symbol]; ok && configured != "y" {
			return nil, fmt.Errorf("%s=%s contradicts Linux target profile %q", symbol, configured, p.Name)
		}
	}
	for symbol, value := range p.ABISeeds {
		if configured, ok := prepared[symbol]; ok && configured != value {
			return nil, fmt.Errorf("%s=%s contradicts immutable ABI value %s for Linux target profile %q", symbol, configured, value, p.Name)
		}
		prepared[symbol] = value
	}
	if p.Name == "armv7" && prepared["CONFIG_64BIT"] == "y" {
		return nil, fmt.Errorf("CONFIG_64BIT=y contradicts 32-bit Linux target profile %q", p.Name)
	}
	return prepared, nil
}

// ValidateResolvedArchitecture ensures Kconfig derived the expected root
// architecture symbol after selecting the architecture tree through ARCH.
func (p LinuxTargetProfile) ValidateResolvedArchitecture(resolved *ResolvedConfig) error {
	want := map[string]string{
		"x86_64":  "CONFIG_X86",
		"aarch64": "CONFIG_ARM64",
		"armv7":   "CONFIG_ARM",
	}[p.Name]
	if resolved.Value(want) != "y" {
		return fmt.Errorf("Kconfig did not derive %s=y for Linux target profile %q (ARCH=%q)", want, p.Name, p.Arch)
	}
	return nil
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
