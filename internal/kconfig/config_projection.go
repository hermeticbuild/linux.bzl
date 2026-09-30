package kconfig

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// recognizedConfigDocuments describes formats understood by symbol filtering.
// It is not an inventory: the selected source's native conf owns which files
// exist, including optional documents and per-symbol dependency markers.
func recognizedConfigDocuments() []string {
	return []string{".config", "include/config/auto.conf", "include/config/auto.conf.cmd", "include/generated/autoconf.h", "include/generated/rustc_cfg"}
}

func nativeConfigArtifactPath(pathname string) bool {
	return canonicalKbuildRulePath(pathname) == pathname && pathname != "" &&
		(pathname == ".config" || strings.HasPrefix(pathname, "include/config/") ||
			strings.HasPrefix(pathname, "include/generated/"))
}

// NativeConfigProjectionPaths validates the native serializer's complete
// file map and returns its canonical inventory. Consumers must use this actual
// inventory rather than infer optional outputs from a kernel version.
func NativeConfigProjectionPaths(files map[string]string) ([]string, error) {
	for _, document := range recognizedConfigDocuments() {
		if document == "include/generated/rustc_cfg" {
			continue
		}
		if _, exists := files[document]; !exists {
			return nil, fmt.Errorf("native config projection is missing %q", document)
		}
	}
	paths := slices.Sorted(maps.Keys(files))
	for _, pathname := range paths {
		if !nativeConfigArtifactPath(pathname) {
			return nil, fmt.Errorf("native config projection has invalid artifact path %q", pathname)
		}
	}
	return paths, nil
}

func actionPlanConfigProjectionPaths(plan *ActionPlan) []string {
	paths := map[string]bool{}
	for _, source := range plan.Sources {
		if source.Namespace == "config" {
			paths[source.Path] = true
		}
	}
	return slices.Sorted(maps.Keys(paths))
}
