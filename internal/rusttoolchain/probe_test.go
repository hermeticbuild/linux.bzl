package rusttoolchain

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseVerboseNightly(t *testing.T) {
	probe, err := ParseVerbose(`rustc 1.98.0-nightly (012345678 2026-06-23)
binary: rustc
commit-hash: 0123456789
commit-date: 2026-06-23
host: x86_64-unknown-linux-gnu
release: 1.98.0-nightly
LLVM version: 22.1.7
`)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Semver != "1.98.0" || probe.Channel != "nightly" || probe.VersionCode != 109800 {
		t.Fatalf("unexpected rustc identity: %#v", probe)
	}
	if probe.VersionText != "rustc 1.98.0-nightly (012345678 2026-06-23)" {
		t.Fatalf("version text = %q", probe.VersionText)
	}
	if probe.LLVMVersionCode != 220107 || probe.CommitDate != "2026-06-23" {
		t.Fatalf("unexpected rustc details: %#v", probe)
	}
	if probe.CommitHash != "0123456789" {
		t.Fatalf("commit hash = %q", probe.CommitHash)
	}
}

func TestParseVerboseStable(t *testing.T) {
	probe, err := ParseVerbose(`rustc 1.78.0 (9b00956e5 2024-04-29)
commit-hash: 9b00956e56009bab2aa15d7bff10916599e3d6d6
release: 1.78.0
LLVM version: 18.1.2
`)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Channel != "stable" || probe.VersionCode != 107800 {
		t.Fatalf("unexpected stable identity: %#v", probe)
	}
	atLeast, err := probe.AtLeast("1.78.0")
	if err != nil || !atLeast {
		t.Fatalf("AtLeast() = %v, %v", atLeast, err)
	}
}

func TestProbeRoundTrip(t *testing.T) {
	probe, err := ParseVerbose(`rustc 1.97.0 (abc 2026-03-01)
commit-hash: abcdef0123456789
release: 1.97.0
LLVM version: 22.1.6
`)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, probe); err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(strings.NewReader(encoded.String()))
	if err != nil {
		t.Fatal(err)
	}
	if decoded != probe {
		t.Fatalf("round trip = %#v, want %#v", decoded, probe)
	}
}

func TestParseVerboseRejectsMissingLLVM(t *testing.T) {
	_, err := ParseVerbose("rustc 1.78.0\nrelease: 1.78.0\n")
	if err == nil {
		t.Fatal("ParseVerbose() succeeded without LLVM version")
	}
}

func TestProbeRejectsMismatchedVersionText(t *testing.T) {
	probe, err := ParseVerbose(`rustc 1.98.0-nightly (012345678 2026-06-23)
commit-hash: 0123456789
release: 1.98.0-nightly
LLVM version: 22.1.7
`)
	if err != nil {
		t.Fatal(err)
	}
	probe.VersionText = "rustc 1.97.0"
	if err := probe.Validate(); err == nil {
		t.Fatal("Probe.Validate() accepted version text for another release")
	}
}

func TestParseVerboseRejectsMissingCommitHash(t *testing.T) {
	_, err := ParseVerbose(`rustc 1.78.0 (9b00956e5 2024-04-29)
release: 1.78.0
LLVM version: 18.1.2
`)
	if err == nil {
		t.Fatal("ParseVerbose() succeeded without commit-hash")
	}
}
