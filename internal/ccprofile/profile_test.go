package ccprofile

import (
	"strings"
	"testing"
)

func validProfile() Profile {
	probe := StructuralProbe{
		Kind:       "cc-option",
		Language:   "c",
		PrefixArgv: []string{"-Werror"},
		Argv:       []string{"-mno-outline-atomics"},
		Supported:  true,
	}
	probe.ID = StructuralProbeID(probe)
	return Profile{
		Schema:         Schema,
		Architecture:   "x86_64",
		DriverContract: DriverContract,
		AnalysisIdentity: AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigIdentity: KconfigIdentity{
			CCName:        "Clang",
			CCVersion:     220108,
			CCVersionText: "clang version 22.1.8",
			ASName:        "LLVM",
			ASVersion:     0,
			LDName:        "LLD",
			LDVersion:     220108,
			CanLink:       true,
			BuiltinMacros: map[string]string{"__SIZEOF_INT128__": "16"},
		},
		StructuralProbes: []StructuralProbe{probe},
	}
}

func TestCanonicalRoundTrip(t *testing.T) {
	profile := validProfile()
	data, err := CanonicalJSON(profile)
	if err != nil {
		t.Fatalf("CanonicalJSON() failed: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode() failed: %v\n%s", err, data)
	}
	if err := Compare(profile, got); err != nil {
		t.Fatalf("round-trip mismatch: %v", err)
	}
	first, err := Digest(profile)
	if err != nil {
		t.Fatalf("Digest() failed: %v", err)
	}
	second, err := Digest(got)
	if err != nil {
		t.Fatalf("round-trip Digest() failed: %v", err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("digests = %q and %q", first, second)
	}
}

func TestDecodeRejectsDuplicateAndUnknownKeys(t *testing.T) {
	data, err := CanonicalJSON(validProfile())
	if err != nil {
		t.Fatal(err)
	}
	duplicate := strings.Replace(string(data), `"schema":`, `"schema": "duplicate", "schema":`, 1)
	if _, err := Decode([]byte(duplicate)); err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("Decode(duplicate) error = %v", err)
	}
	unknown := strings.Replace(string(data), `"architecture":`, `"unknown": 1, "architecture":`, 1)
	if _, err := Decode([]byte(unknown)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Decode(unknown) error = %v", err)
	}
}

func TestDecodeRequiresFalseAndZeroFields(t *testing.T) {
	data, err := CanonicalJSON(validProfile())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"cc_version": 220108,`, `"as_version": 0,`, `"can_link": true,`} {
		broken := strings.Replace(string(data), field, "", 1)
		if _, err := Decode([]byte(broken)); err == nil || !strings.Contains(err.Error(), "is required") {
			t.Fatalf("Decode(without %s) error = %v", field, err)
		}
	}
}

func TestValidateRejectsProbeAndIdentityMismatch(t *testing.T) {
	profile := validProfile()
	profile.StructuralProbes[0].ID = strings.Repeat("0", 64)
	if err := Validate(profile); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("Validate(bad probe ID) error = %v", err)
	}
	profile = validProfile()
	profile.KconfigIdentity.CCName = "GCC"
	if err := Validate(profile); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Validate(identity mismatch) error = %v", err)
	}
}

func TestStructuralProbeIDIncludesPrefixBoundary(t *testing.T) {
	left := StructuralProbe{Kind: "cc-option", Language: "c", PrefixArgv: []string{"a"}, Argv: []string{"b"}}
	right := StructuralProbe{Kind: "cc-option", Language: "c", Argv: []string{"a", "b"}}
	if StructuralProbeID(left) == StructuralProbeID(right) {
		t.Fatal("StructuralProbeID does not distinguish prefix boundary")
	}
}

func TestValidateStructuralProbeCoverageRejectsMissingRequest(t *testing.T) {
	profile := validProfile()
	missing := StructuralProbe{
		Kind:     "ld-option",
		Language: "link",
		Argv:     []string{"--build-id"},
	}
	missing.ID = StructuralProbeID(missing)
	requests := append([]StructuralProbe{}, profile.StructuralProbes...)
	requests = append(requests, missing)
	if requests[0].ID > requests[1].ID {
		requests[0], requests[1] = requests[1], requests[0]
	}

	err := ValidateStructuralProbeCoverage(profile, requests)
	if err == nil || !strings.Contains(err.Error(), "missing structural probe request") {
		t.Fatalf("ValidateStructuralProbeCoverage() error = %v, want missing request", err)
	}
}
