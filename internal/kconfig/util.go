package kconfig

import (
	"path/filepath"
	"sort"
	"strings"
)

func labelFor(labelPackage, target string) string {
	if labelPackage == "" {
		return "//:" + target
	}
	if strings.HasPrefix(labelPackage, "@") || strings.HasPrefix(labelPackage, "//") {
		return strings.TrimSuffix(labelPackage, ":") + ":" + target
	}
	return "//" + labelPackage + ":" + target
}

func mappedSourceRootPath(path string, roots map[string]string) (string, bool) {
	if len(roots) == 0 {
		return "", false
	}
	prefixes := make([]string, 0, len(roots))
	for prefix := range roots {
		prefix = strings.Trim(filepath.ToSlash(prefix), "/")
		if prefix != "" && prefix != "." {
			prefixes = append(prefixes, prefix)
		}
	}
	sort.Slice(prefixes, func(i, j int) bool {
		if len(prefixes[i]) == len(prefixes[j]) {
			return prefixes[i] < prefixes[j]
		}
		return len(prefixes[i]) > len(prefixes[j])
	})
	path = filepath.ToSlash(path)
	for _, prefix := range prefixes {
		if path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		rel := strings.TrimPrefix(path, prefix)
		rel = strings.TrimPrefix(rel, "/")
		return filepath.Join(roots[prefix], filepath.FromSlash(rel)), true
	}
	return "", false
}

func allowedValues(typ SymbolType) []string {
	switch typ {
	case SymbolBool:
		return []string{"y", "n"}
	case SymbolTristate:
		return []string{"y", "m", "n"}
	default:
		return nil
	}
}
