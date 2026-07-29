package kconfig

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/ccprofile"
)

func mustParseString(t *testing.T, fixture string) *Tree {
	t.Helper()
	tree, err := Parse(context.Background(), strings.NewReader(fixture), "Kconfig", Options{})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	return tree
}

func testCCProfile(probes ...ccprofile.StructuralProbe) *ccprofile.Profile {
	if probes == nil {
		probes = []ccprofile.StructuralProbe{}
	}
	for index := range probes {
		probes[index].ID = ccprofile.StructuralProbeID(probes[index])
	}
	sort.Slice(probes, func(i, j int) bool {
		return probes[i].ID < probes[j].ID
	})
	return &ccprofile.Profile{
		Schema:         ccprofile.Schema,
		Architecture:   "x86_64",
		DriverContract: ccprofile.DriverContract,
		AnalysisIdentity: ccprofile.AnalysisIdentity{
			Compiler:            "clang",
			TargetGNUSystemName: "x86_64-unknown-linux-gnu",
		},
		KconfigIdentity: ccprofile.KconfigIdentity{
			CCName:        "Clang",
			CCVersion:     220108,
			CCVersionText: "clang version 22.1.8",
			ASName:        "LLVM",
			LDName:        "LLD",
			LDVersion:     220108,
			CanLink:       true,
			BuiltinMacros: map[string]string{"__SIZEOF_INT128__": "16"},
		},
		StructuralProbes: probes,
	}
}
