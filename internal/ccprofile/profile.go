// Package ccprofile defines the source capability profile shared by repository
// generation and Bazel actions.
package ccprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
)

const (
	Schema                    = "linux.bzl/cc-capability-profile-v1"
	DriverContract            = "gnu-cc-response-v1"
	StructuralProbeSourceRoot = "__LINUX_BZL_SRCTREE__"
	StructuralProbeObjectRoot = "__LINUX_BZL_OBJTREE__"
)

var macroNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Profile struct {
	Schema           string            `json:"schema"`
	Architecture     string            `json:"architecture"`
	DriverContract   string            `json:"driver_contract"`
	AnalysisIdentity AnalysisIdentity  `json:"analysis_identity"`
	KconfigIdentity  KconfigIdentity   `json:"kconfig_identity"`
	StructuralProbes []StructuralProbe `json:"structural_probes"`
}

type AnalysisIdentity struct {
	Compiler            string `json:"compiler"`
	TargetGNUSystemName string `json:"target_gnu_system_name"`
}

type KconfigIdentity struct {
	CCName        string            `json:"cc_name"`
	CCVersion     int               `json:"cc_version"`
	CCVersionText string            `json:"cc_version_text"`
	ASName        string            `json:"as_name"`
	ASVersion     int               `json:"as_version"`
	LDName        string            `json:"ld_name"`
	LDVersion     int               `json:"ld_version"`
	CanLink       bool              `json:"can_link"`
	BuiltinMacros map[string]string `json:"builtin_macros"`
}

type StructuralProbe struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Language   string   `json:"language"`
	PrefixArgv []string `json:"prefix_argv"`
	Argv       []string `json:"argv"`
	Supported  bool     `json:"supported"`
}

func StructuralProbeID(probe StructuralProbe) string {
	hash := sha256.New()
	hash.Write([]byte("linux.bzl/cc-structural-probe/v1\x00"))
	hash.Write([]byte(probe.Kind))
	hash.Write([]byte{0})
	hash.Write([]byte(probe.Language))
	hash.Write([]byte{0})
	for _, arg := range probe.PrefixArgv {
		hash.Write([]byte(arg))
		hash.Write([]byte{0})
	}
	hash.Write([]byte{0})
	for _, arg := range probe.Argv {
		hash.Write([]byte(arg))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func Validate(profile Profile) error {
	if profile.Schema != Schema {
		return fmt.Errorf("schema %q, want %q", profile.Schema, Schema)
	}
	if err := validateArchitecture(profile.Architecture); err != nil {
		return err
	}
	if profile.DriverContract != DriverContract {
		return fmt.Errorf("driver_contract %q, want %q", profile.DriverContract, DriverContract)
	}
	if err := validateAnalysisIdentity(profile.AnalysisIdentity); err != nil {
		return err
	}
	expectedCCName := "Clang"
	if profile.AnalysisIdentity.Compiler == "gcc" {
		expectedCCName = "GCC"
	}
	if profile.KconfigIdentity.CCName != expectedCCName {
		return fmt.Errorf(
			"kconfig_identity.cc_name %q does not match compiler %q",
			profile.KconfigIdentity.CCName,
			profile.AnalysisIdentity.Compiler,
		)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"kconfig_identity.cc_version_text", profile.KconfigIdentity.CCVersionText},
		{"kconfig_identity.as_name", profile.KconfigIdentity.ASName},
		{"kconfig_identity.ld_name", profile.KconfigIdentity.LDName},
	} {
		if err := validateText(field.value, field.name); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name  string
		value int
	}{
		{"kconfig_identity.cc_version", profile.KconfigIdentity.CCVersion},
		{"kconfig_identity.as_version", profile.KconfigIdentity.ASVersion},
		{"kconfig_identity.ld_version", profile.KconfigIdentity.LDVersion},
	} {
		if field.value < 0 {
			return fmt.Errorf("%s must be non-negative", field.name)
		}
	}
	for name, value := range profile.KconfigIdentity.BuiltinMacros {
		if !macroNamePattern.MatchString(name) {
			return fmt.Errorf("invalid builtin macro name %q", name)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("builtin macro %s contains NUL", name)
		}
	}
	previousID := ""
	for index, probe := range profile.StructuralProbes {
		context := fmt.Sprintf("structural_probes[%d]", index)
		if err := validateProbe(probe, context); err != nil {
			return err
		}
		if probe.ID <= previousID {
			return fmt.Errorf("structural_probes must be sorted by unique ID")
		}
		previousID = probe.ID
	}
	return nil
}

func validateProbe(probe StructuralProbe, context string) error {
	kinds := map[string]bool{
		"as-option":          true,
		"cc-disable-warning": true,
		"cc-option":          true,
		"cc-option-yn":       true,
		"ld-option":          true,
	}
	if !kinds[probe.Kind] {
		return fmt.Errorf("%s has unsupported kind %q", context, probe.Kind)
	}
	languages := map[string]bool{"asm": true, "c": true, "link": true}
	if !languages[probe.Language] {
		return fmt.Errorf("%s has unsupported language %q", context, probe.Language)
	}
	if probe.Kind == "as-option" && probe.Language != "asm" {
		return fmt.Errorf("%s as-option requires asm language", context)
	}
	if probe.Kind == "ld-option" && probe.Language != "link" {
		return fmt.Errorf("%s ld-option requires link language", context)
	}
	if probe.Kind != "as-option" && probe.Kind != "ld-option" && probe.Language != "c" {
		return fmt.Errorf("%s %s requires c language", context, probe.Kind)
	}
	if len(probe.Argv) == 0 {
		return fmt.Errorf("%s argv must not be empty", context)
	}
	for _, values := range []struct {
		name string
		args []string
	}{
		{"prefix_argv", probe.PrefixArgv},
		{"argv", probe.Argv},
	} {
		for index, arg := range values.args {
			if arg == "" || strings.ContainsRune(arg, '\x00') {
				return fmt.Errorf("%s %s[%d] must be non-empty and contain no NUL", context, values.name, index)
			}
		}
	}
	if expected := StructuralProbeID(probe); probe.ID != expected {
		return fmt.Errorf("%s ID %q, want %q", context, probe.ID, expected)
	}
	return nil
}

func validateText(value, name string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s contains NUL", name)
	}
	return nil
}

func CanonicalJSON(profile Profile) ([]byte, error) {
	if profile.KconfigIdentity.BuiltinMacros == nil {
		profile.KconfigIdentity.BuiltinMacros = map[string]string{}
	}
	if profile.StructuralProbes == nil {
		profile.StructuralProbes = []StructuralProbe{}
	}
	probes := make([]StructuralProbe, len(profile.StructuralProbes))
	copy(probes, profile.StructuralProbes)
	profile.StructuralProbes = probes
	for index := range profile.StructuralProbes {
		if profile.StructuralProbes[index].PrefixArgv == nil {
			profile.StructuralProbes[index].PrefixArgv = []string{}
		}
		if profile.StructuralProbes[index].Argv == nil {
			profile.StructuralProbes[index].Argv = []string{}
		}
	}
	sort.Slice(profile.StructuralProbes, func(i, j int) bool {
		return profile.StructuralProbes[i].ID < profile.StructuralProbes[j].ID
	})
	if err := Validate(profile); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode CC profile: %w", err)
	}
	return append(data, '\n'), nil
}

func Digest(profile Profile) (string, error) {
	data, err := CanonicalJSON(profile)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("linux.bzl/cc-capability-profile/v1\x00"), data...))
	return hex.EncodeToString(sum[:]), nil
}

func ValidateStructuralProbeCoverage(
	profile Profile,
	requests []StructuralProbe,
) error {
	if err := Validate(profile); err != nil {
		return err
	}
	previousID := ""
	for index, request := range requests {
		if err := validateProbe(request, fmt.Sprintf("requests[%d]", index)); err != nil {
			return err
		}
		if request.ID <= previousID {
			return fmt.Errorf("structural probe requests must be sorted by unique ID")
		}
		previousID = request.ID
	}

	probesByID := make(map[string]StructuralProbe, len(profile.StructuralProbes))
	for _, probe := range profile.StructuralProbes {
		probesByID[probe.ID] = probe
	}
	requestsByID := make(map[string]bool, len(requests))
	for _, request := range requests {
		requestsByID[request.ID] = true
		probe, ok := probesByID[request.ID]
		if !ok {
			return fmt.Errorf("missing structural probe request %s", request.ID)
		}
		if probe.Kind != request.Kind ||
			probe.Language != request.Language ||
			!slices.Equal(probe.PrefixArgv, request.PrefixArgv) ||
			!slices.Equal(probe.Argv, request.Argv) {
			return fmt.Errorf("structural probe request %s semantic mismatch", request.ID)
		}
	}
	for _, probe := range profile.StructuralProbes {
		if !requestsByID[probe.ID] {
			return fmt.Errorf("unexpected structural probe %s", probe.ID)
		}
	}
	return nil
}

func Compare(expected, actual Profile) error {
	if reflect.DeepEqual(expected, actual) {
		return nil
	}
	if expected.Schema != actual.Schema {
		return fmt.Errorf("schema mismatch: expected %q, got %q", expected.Schema, actual.Schema)
	}
	if expected.Architecture != actual.Architecture {
		return fmt.Errorf("architecture mismatch: expected %q, got %q", expected.Architecture, actual.Architecture)
	}
	if expected.DriverContract != actual.DriverContract {
		return fmt.Errorf("driver_contract mismatch: expected %q, got %q", expected.DriverContract, actual.DriverContract)
	}
	if !reflect.DeepEqual(expected.AnalysisIdentity, actual.AnalysisIdentity) {
		return fmt.Errorf("analysis_identity mismatch: expected %#v, got %#v", expected.AnalysisIdentity, actual.AnalysisIdentity)
	}
	if !reflect.DeepEqual(expected.KconfigIdentity, actual.KconfigIdentity) {
		return fmt.Errorf("kconfig_identity mismatch: expected %#v, got %#v", expected.KconfigIdentity, actual.KconfigIdentity)
	}
	return fmt.Errorf("structural_probes mismatch")
}
