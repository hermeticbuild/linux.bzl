package main

import (
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/rusttoolchain"
)

func TestApplyRustToolchainProbe(t *testing.T) {
	vars := stringMapFlag{"RUSTC_VERSION_TEXT": "repository baseline"}
	probeValues := stringMapFlag{}
	applyRustToolchainProbe(vars, probeValues, rusttoolchain.Probe{
		VersionText:     "rustc 1.98.0-nightly (012345678 2026-06-24)",
		VersionCode:     109800,
		LLVMVersionCode: 220107,
	})
	if got, want := vars["RUSTC_VERSION_TEXT"], "rustc 1.98.0-nightly (012345678 2026-06-24)"; got != want {
		t.Fatalf("RUSTC_VERSION_TEXT = %q, want %q", got, want)
	}
	if got, want := probeValues["rustc_version"], "109800"; got != want {
		t.Fatalf("rustc_version = %q, want %q", got, want)
	}
	if got, want := probeValues["rustc_llvm_version"], "220107"; got != want {
		t.Fatalf("rustc_llvm_version = %q, want %q", got, want)
	}
}

func TestGeneratedIncludeFlag(t *testing.T) {
	values := generatedIncludeFlag{}
	for _, value := range []string{
		"generated/autoconf.h",
		"asm/rwonce.h=include/asm-generic/rwonce.h",
		"asm/errno.h=include/uapi/asm-generic/errno.h",
	} {
		if err := values.Set(value); err != nil {
			t.Fatalf("Set(%q) failed: %v", value, err)
		}
	}
	if got := values["generated/autoconf.h"]; got != nil {
		t.Fatalf("generated/autoconf.h backings = %v, want nil", got)
	}
	if got, want := strings.Join(values["asm/rwonce.h"], ","), "include/asm-generic/rwonce.h"; got != want {
		t.Fatalf("asm/rwonce.h backings = %q, want %q", got, want)
	}
	if err := values.Set("missing="); err == nil {
		t.Fatal("Set(missing=) succeeded")
	}
}

func TestKbuildVariablesForConfigUsesWrittenConfigView(t *testing.T) {
	vars := kbuildVariablesForConfig(
		map[string]string{
			"ARCH":        "arm64",
			"CONFIG_BASE": "base",
		},
		&kconfig.ResolvedConfig{
			Effective: map[string]string{
				"CONFIG_DISABLED": "n",
				"CONFIG_HIDDEN":   "y",
				"CONFIG_MODULE":   "m",
				"CONFIG_WRITTEN":  "y",
			},
			Written: map[string]bool{
				"CONFIG_MODULE":  true,
				"CONFIG_WRITTEN": true,
			},
		},
	)

	for key, want := range map[string]string{
		"ARCH":            "arm64",
		"CONFIG_BASE":     "base",
		"CONFIG_DISABLED": "",
		"CONFIG_HIDDEN":   "",
		"CONFIG_MODULE":   "m",
		"CONFIG_WRITTEN":  "y",
	} {
		if got := vars[key]; got != want {
			t.Fatalf("vars[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestResolvedFlagLinesUsesWrittenConfigView(t *testing.T) {
	resolved := &kconfig.ResolvedConfig{
		Effective: map[string]string{
			"CONFIG_Z":        "y",
			"CONFIG_A":        "m",
			"CONFIG_DISABLED": "n",
			"CONFIG_HIDDEN":   "y",
		},
		Written: map[string]bool{
			"CONFIG_A":        true,
			"CONFIG_DISABLED": true,
			"CONFIG_Z":        true,
		},
	}
	got := strings.Join(resolvedFlagLines(resolved), "\n")
	if want := "CONFIG_A=m\nCONFIG_Z=y"; got != want {
		t.Fatalf("resolvedFlagLines() = %q, want %q", got, want)
	}
}

func TestRustcCfgLinesMatchKernelEncoding(t *testing.T) {
	tree, err := kconfig.Parse(
		t.Context(),
		strings.NewReader(`
config BOOL
	bool
config TRI
	tristate
config STR
	string
config EMPTY
	string
config INT
	int
config HEX
	hex
config HEX_PREFIXED
	hex
`),
		"Kconfig",
		kconfig.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	resolved := &kconfig.ResolvedConfig{
		Effective: map[string]string{
			"CONFIG_BOOL":         "y",
			"CONFIG_EMPTY":        "",
			"CONFIG_TRI":          "m",
			"CONFIG_STR":          `"quoted \"value\""`,
			"CONFIG_INT":          "42",
			"CONFIG_HEX":          "2a",
			"CONFIG_HEX_PREFIXED": "0X2A",
		},
		Written: map[string]bool{
			"CONFIG_BOOL":         true,
			"CONFIG_EMPTY":        true,
			"CONFIG_TRI":          true,
			"CONFIG_STR":          true,
			"CONFIG_INT":          true,
			"CONFIG_HEX":          true,
			"CONFIG_HEX_PREFIXED": true,
		},
	}
	got := strings.Join(rustcCfgLines(tree, resolved), "\n")
	want := strings.Join([]string{
		`--cfg=CONFIG_BOOL`,
		`--cfg=CONFIG_BOOL="y"`,
		`--cfg=CONFIG_EMPTY=""`,
		`--cfg=CONFIG_HEX="0x2a"`,
		`--cfg=CONFIG_HEX_PREFIXED="0X2A"`,
		`--cfg=CONFIG_INT="42"`,
		`--cfg=CONFIG_STR="quoted \"value\""`,
		`--cfg=CONFIG_TRI`,
		`--cfg=CONFIG_TRI="m"`,
	}, "\n")
	if got != want {
		t.Fatalf("rustcCfgLines() =\n%s\nwant:\n%s", got, want)
	}
}

func TestCompactMetadataRejectsUnmatchedOverlay(t *testing.T) {
	_, err := compactMetadata(
		nil,
		"",
		"Kbuild",
		[]namedPath{{Name: "base", Path: "base.config"}},
		[]namedPath{{Name: "other", Path: "overlay.config"}},
		"",
		"default",
		false,
		nil,
		nil,
		nil,
		nil,
		false,
		kconfig.CompactSchemaV012,
	)
	if err == nil || !strings.Contains(err.Error(), `config overlay "other" has no matching -config`) {
		t.Fatalf("compactMetadata() error = %v, want unmatched overlay", err)
	}
}

func TestCompactMetadataRejectsDuplicateOverlay(t *testing.T) {
	_, err := compactMetadata(
		nil,
		"",
		"Kbuild",
		[]namedPath{{Name: "base", Path: "base.config"}},
		[]namedPath{
			{Name: "base", Path: "first.config"},
			{Name: "base", Path: "second.config"},
		},
		"",
		"default",
		false,
		nil,
		nil,
		nil,
		nil,
		false,
		kconfig.CompactSchemaV012,
	)
	if err == nil || !strings.Contains(err.Error(), `duplicate config overlay name "base"`) {
		t.Fatalf("compactMetadata() error = %v, want duplicate overlay", err)
	}
}

func TestCompactMetadataRejectsUnsafeResolvedFlagsNames(t *testing.T) {
	for _, name := range []string{"../outside", "nested/config", `nested\config`, ".", ".."} {
		t.Run(name, func(t *testing.T) {
			_, err := compactMetadata(
				nil,
				"",
				"Kbuild",
				[]namedPath{{Name: name, Path: "base.config"}},
				nil,
				t.TempDir(),
				"default",
				false,
				nil,
				nil,
				nil,
				nil,
				false,
				kconfig.CompactSchemaV012,
			)
			if err == nil || !strings.Contains(err.Error(), "must be a single path component") {
				t.Fatalf("compactMetadata() error = %v, want unsafe name rejection", err)
			}
		})
	}
}
