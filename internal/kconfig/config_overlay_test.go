// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kconfig

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseConfigOverlay(t *testing.T) {
	overlay, err := ParseConfigOverlay(strings.NewReader(
		"CONFIG_KUNIT=y\n" +
			"# CONFIG_DEBUG is not set\n" +
			"CONFIG_HZ=250\n" +
			"# a plain comment, not an unset directive\n",
	))
	if err != nil {
		t.Fatalf("ParseConfigOverlay() failed: %v", err)
	}
	want := map[string]string{
		"CONFIG_KUNIT": "y",
		"CONFIG_DEBUG": "n",
		"CONFIG_HZ":    "250",
	}
	if !reflect.DeepEqual(overlay, want) {
		t.Fatalf("ParseConfigOverlay() = %#v, want %#v", overlay, want)
	}
}

func TestParseConfigOverlayRejectsDuplicates(t *testing.T) {
	_, err := ParseConfigOverlay(strings.NewReader("CONFIG_DEBUG=y\n# CONFIG_DEBUG is not set\n"))
	if err == nil || !strings.Contains(err.Error(), `duplicate config key "CONFIG_DEBUG"`) {
		t.Fatalf("ParseConfigOverlay() error = %v, want duplicate CONFIG_DEBUG", err)
	}
}

func TestParseConfigOverlayRejectsMalformedAssignment(t *testing.T) {
	_, err := ParseConfigOverlay(strings.NewReader("not an assignment\n"))
	if err == nil || !strings.Contains(err.Error(), "expected CONFIG_* assignment") {
		t.Fatalf("ParseConfigOverlay() error = %v, want malformed assignment", err)
	}
}

func TestMergeConfigOverlayWins(t *testing.T) {
	base := map[string]string{"CONFIG_A": "y", "CONFIG_B": "y"}
	MergeConfigOverlay(base, map[string]string{"CONFIG_B": "n", "CONFIG_C": "y"})
	want := map[string]string{"CONFIG_A": "y", "CONFIG_B": "n", "CONFIG_C": "y"}
	if !reflect.DeepEqual(base, want) {
		t.Fatalf("MergeConfigOverlay() = %#v, want %#v", base, want)
	}
}

func TestResolveConfigWithOverlay(t *testing.T) {
	fixture := `
mainmenu "Test"

config FEATURE
	bool "Feature"
	select FEATURE_DEP

config FEATURE_DEP
	bool "Feature dependency"

config OPTIONAL
	bool "Optional"

config DEFAULT_ON
	bool "Default on"
	default y
`
	base, err := ParseConfig(strings.NewReader("CONFIG_OPTIONAL=y\n"))
	if err != nil {
		t.Fatalf("ParseConfig() failed: %v", err)
	}
	overlay, err := ParseConfigOverlay(strings.NewReader(
		"CONFIG_FEATURE=y\n" +
			"# CONFIG_OPTIONAL is not set\n" +
			"# CONFIG_DEFAULT_ON is not set\n",
	))
	if err != nil {
		t.Fatalf("ParseConfigOverlay() failed: %v", err)
	}
	MergeConfigOverlay(base, overlay)

	resolved := mustResolveConfig(t, fixture, base)
	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_FEATURE":     "y",
		"CONFIG_FEATURE_DEP": "y",
		"CONFIG_OPTIONAL":    "n",
		"CONFIG_DEFAULT_ON":  "n",
	})
}
