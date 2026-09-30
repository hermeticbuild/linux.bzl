package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// A selected Make invocation can explicitly include an independently produced
// FEATURES_DUMP. The virtual include is a private analysis input: the normal
// Makefile.feature branch is not executed, and its individual source-defined
// feature checks are measured before the selected invocation is replayed.
type kbuildSelectedFeatureDump struct {
	scopes                    *kconfig.KbuildProbeScopes
	results                   *kconfig.KbuildGraphGuardResults
	discoveryOnly             bool
	sourceOutputDiscoveryOnly bool
	requestIDs                *[]string
}

type pendingKbuildFeatureDump struct {
	makefile   string
	requestIDs []string
}

func selectedFeatureDumpProbePass(
	scopes *kconfig.KbuildProbeScopes,
	results *kconfig.KbuildGraphGuardResults,
	sourceOutputDiscoveryOnly, featureDumpDiscoveryOnly bool,
	requestIDs *[]string,
) *kbuildSelectedFeatureDump {
	return &kbuildSelectedFeatureDump{
		scopes: scopes, results: results,
		discoveryOnly: sourceOutputDiscoveryOnly || featureDumpDiscoveryOnly,
		// Source discovery must stop at an unmeasured feature include; feature
		// discovery must first register the include's selected compiler checks.
		sourceOutputDiscoveryOnly: sourceOutputDiscoveryOnly,
		requestIDs:                requestIDs,
	}
}

func selectedFeatureDumpSourceCut(measurement *kbuildSelectedFeatureDump, makefile string) error {
	if measurement == nil {
		return fmt.Errorf("source-selected feature dump %q has no measured compiler contract", makefile)
	}
	if measurement.sourceOutputDiscoveryOnly && measurement.results == nil {
		return &pendingKbuildFeatureDump{makefile: makefile}
	}
	return nil
}

func (pending *pendingKbuildFeatureDump) Error() string {
	return fmt.Sprintf("selected feature dump for %q awaits measured compiler statuses", pending.makefile)
}

func selectedKbuildFeatureDumpPath(root, makefile string, request kbuildInvocationRequest) (string, bool, error) {
	selected, err := kconfig.SelectedFeatureDumpSourceAlternate(root, makefile)
	if err != nil {
		return "", false, fmt.Errorf("inspect selected Makefile %q for feature dump: %w", makefile, err)
	}
	if !selected {
		return "", false, nil
	}
	if _, overridden := request.variables["FEATURES_DUMP"]; overridden {
		return "", false, fmt.Errorf("selected Make invocation %q already has a FEATURES_DUMP assignment", request.name)
	}
	// The same source invocation and incoming Make frontier must name the same
	// virtual include in discovery and replay. Keep it within the declared
	// object namespace, away from any source-created OUTPUT file.
	digest := canonicalKbuildInvocationRequestDigest(request)
	return path.Join(".linux-bzl-feature-dumps", hex.EncodeToString(digest[:])+".mk"), true, nil
}

func measureSelectedKbuildFeatureDump(
	root, makefile, dumpPath string,
	profile kconfig.CompactKbuildProfile,
	measurement *kbuildSelectedFeatureDump,
) (string, error) {
	if measurement == nil || measurement.scopes == nil {
		return "", fmt.Errorf("selected feature dump %q has no configured probe scopes", makefile)
	}
	selected, err := kconfig.EvaluateCompactKbuildTextSymbolic(
		profile, "", "", nil, nil, nil, "$(FEATURE_TESTS)",
	)
	if err != nil {
		return "", fmt.Errorf("evaluate selected FEATURE_TESTS for %q: %w", makefile, err)
	}
	features := strings.Fields(selected)
	probes, err := kconfig.DeriveSelectedFeatureDumpProbeCommands(root, makefile, profile, features)
	if err != nil {
		return "", fmt.Errorf("authenticate selected feature dump for %q: %w", makefile, err)
	}
	if len(probes) == 0 {
		return "", fmt.Errorf("selected Makefile %q mentions FEATURES_DUMP without an authenticated alternate include", makefile)
	}
	var contents strings.Builder
	requestIDs := []string{}
	pending := false
	for _, probe := range probes {
		if probe.DumpPath != dumpPath {
			return "", fmt.Errorf("selected feature dump for %q changed its declared object-tree include", makefile)
		}
		value, ids, err := measurement.scopes.MeasureSelectedFeatureDumpStatus(
			context.Background(), probe.Command, measurement.results, measurement.discoveryOnly,
		)
		if err != nil {
			return "", fmt.Errorf("measure selected feature %q from %q: %w", probe.Feature, makefile, err)
		}
		requestIDs = append(requestIDs, ids...)
		if measurement.discoveryOnly && value != "0" && value != "1" {
			pending = true
		}
		if !pending {
			contents.WriteString("feature-")
			contents.WriteString(probe.Feature)
			contents.WriteByte('=')
			contents.WriteString(value)
			contents.WriteByte('\n')
		}
	}
	requestIDs = uniquePathsInOrder(requestIDs)
	if len(requestIDs) == 0 {
		return "", fmt.Errorf("selected feature dump %q has no measured compiler producers", makefile)
	}
	if measurement.discoveryOnly && pending {
		if measurement.requestIDs != nil {
			*measurement.requestIDs = append(*measurement.requestIDs, requestIDs...)
		}
		return "", &pendingKbuildFeatureDump{makefile: makefile, requestIDs: slices.Clone(requestIDs)}
	}
	return contents.String(), nil
}
