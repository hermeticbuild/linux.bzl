package main

import (
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestLinuxKbuildGuardDiscoveryRequiresReplayCoordinator(t *testing.T) {
	// Reject incomplete mode selection before source/tool loading.
	for _, missing := range []string{"oracle", "family", "coordinator"} {
		t.Run(missing, func(t *testing.T) {
			options := linuxKbuildProbeOptions{
				guardDiscoveryOnly:   true,
				familyVariantOptions: &kconfig.ActionPlanFamilyVariantPlanningOptions{},
				familyCompilerGuards: &familyCompilerGuardPipeline{},
			}
			oracle := new(kconfig.ProbeResultOracle)
			switch missing {
			case "oracle":
				oracle = nil
			case "family":
				options.familyVariantOptions = nil
			case "coordinator":
				options.familyCompilerGuards = nil
			}
			result, err := evaluateLinuxKbuildProbes(options, oracle)
			if result != nil || err == nil || !strings.Contains(err.Error(), "compiler guard discovery requires") {
				t.Fatalf("guard-only evaluation accepted incomplete replay: %#v/%v", result, err)
			}
		})
	}
}
