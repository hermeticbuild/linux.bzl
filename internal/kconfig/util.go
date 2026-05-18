// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kconfig

import "strings"

func labelFor(labelPackage, target string) string {
	if labelPackage == "" {
		return "//:" + target
	}
	if strings.HasPrefix(labelPackage, "@") || strings.HasPrefix(labelPackage, "//") {
		return strings.TrimSuffix(labelPackage, ":") + ":" + target
	}
	return "//" + labelPackage + ":" + target
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
