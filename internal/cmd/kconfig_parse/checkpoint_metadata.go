package main

import (
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// Keep checkpoint rebinding and fresh lowering on one configured contract.
func linuxCompactMetadataOptions(vars, sourceNamespaces map[string]string, objectRoot string, selectedProductsOnly bool, targetContract, hostContract *hostKbuildContract) kconfig.CompactMetadataOptions {
	actionRoles := func(scope string, contract *hostKbuildContract) []kconfig.KbuildActionRoleRef {
		roles := make([]kconfig.KbuildActionRoleRef, 0, len(contract.Actions))
		for role := range contract.Actions {
			roles = append(roles, kconfig.KbuildActionRoleRef{Scope: scope, Role: role})
		}
		sort.Slice(roles, func(i, j int) bool { return roles[i].Role < roles[j].Role })
		return roles
	}
	actionContracts := func(scope string, contract *hostKbuildContract) map[kconfig.KbuildActionRoleRef]kconfig.CompactKbuildActionContract {
		contracts := make(map[kconfig.KbuildActionRoleRef]kconfig.CompactKbuildActionContract, len(contract.Actions))
		for role, action := range contract.Actions {
			contracts[kconfig.KbuildActionRoleRef{Scope: scope, Role: role}] = kconfig.CompactKbuildActionContract{
				PrefixArguments: slices.Clone(action.PrefixArgs),
				SuffixArguments: slices.Clone(action.SuffixArgs),
				Environment:     maps.Clone(action.Environment),
			}
		}
		return contracts
	}
	configuredContracts := actionContracts("target", targetContract)
	for ref, contract := range actionContracts("host", hostContract) {
		configuredContracts[ref] = contract
	}
	// Preserve whether the caller supplied an object tree independently of its
	// normalized path. An explicitly supplied tree may alias the source root but
	// still exposes caller-owned bytes which generated-content probes do not
	// declare.
	preconfiguredObjectTree := objectRoot != ""
	opts := kconfig.CompactMetadataOptions{
		SourceNamespaces: maps.Clone(sourceNamespaces),
		ActionRoles: append(
			actionRoles("target", targetContract),
			actionRoles("host", hostContract)...,
		),
		ActionContracts:         configuredContracts,
		PreconfiguredObjectTree: preconfiguredObjectTree,
		SelectedProductsOnly:    selectedProductsOnly,
	}
	if rustSourceRoot := strings.TrimSpace(vars["RUST_LIB_SRC"]); rustSourceRoot != "" {
		if opts.SourceNamespaces == nil {
			opts.SourceNamespaces = map[string]string{}
		}
		opts.SourceNamespaces[rustSourceRoot] = "rust"
	}
	return opts
}
