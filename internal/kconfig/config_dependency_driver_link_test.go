package kconfig

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func configDependencySourceDriverLinkPlanForTest(t *testing.T, source string, files map[string]string, extraArguments []string, externalHeaders bool) (*ActionPlan, ActionPlanNode) {
	t.Helper()
	arguments := append(slices.Clone(extraArguments), "-o", "lib/crc/helper", "${tree:kernel}/"+source)
	plan, node := configDependencyCompilePlanForTest(t, files, arguments, map[string]string{"CONFIG_CRC_TABLE": "y"})
	plan.Sources[0].Path = source
	node.Stage = "host"
	node.Outputs = []ActionPlanOutput{{Tree: "host", Path: "lib/crc/helper"}}
	node.Trees = []string{"kernel"}
	recipe := plan.Recipes[node.Recipe]
	recipe.Tool = compactKbuildScriptRunnerRole
	recipe.Arguments = []string{"-script_content_base64", "opaque-bounded-script"}
	recipe.Trees = []string{"kernel"}
	recipe.CompilerInvocation = &ActionRecipeCompilerInvocation{
		Tool: "cc", Arguments: arguments,
		WorkingInputUses: []string{"source:source:00000000"}, WorkingInputUsesComplete: true,
	}
	recipe.Sources = []string{"source:00000000", "working-closure:00000001"}
	recipe.WorkingInputs = map[string]string{
		"source:source:00000000":          source,
		"source:working-closure:00000001": "include/generated/autoconf.h",
	}
	plan.Sources = append(plan.Sources, ActionPlanSource{ID: "src-00000002", Namespace: "config", Path: "autoconf.h"})
	node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "working-closure", SourceID: "src-00000002"})
	plan.Recipes[node.Recipe] = recipe
	plan.Nodes[0] = node
	for name, profile := range plan.selectionGraph.profiles {
		if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{Tree: CompactKbuildInvocationObjectTree}); err != nil {
			t.Fatal(err)
		}
		plan.selectionGraph.profiles[name] = profile
		selection := compactKbuildSelectionKey{profile: name, target: "lib/crc/helper", stage: "host"}
		plan.selectionGraph.selections = map[compactKbuildSelectionKey]CompactKbuildSelection{selection: {Profile: name}}
		plan.selectionGraph.materializedProducers = map[compactKbuildSelectionKey]string{selection: node.ID}
	}
	role, ok := toolaction.LinkContractRole("cc")
	if !ok {
		t.Fatal("cc has no driver-link contract")
	}
	ref := KbuildActionRoleRef{Scope: "host", Role: role}
	contract := CompactKbuildActionContract{}
	if externalHeaders {
		contract.SuffixArguments = []string{"-isystem", "/configured/libc/include"}
	}
	baseRef := KbuildActionRoleRef{Scope: "host", Role: "cc"}
	plan.metadata.actionRoles = []KbuildActionRoleRef{baseRef, ref}
	plan.metadata.actionContracts = map[KbuildActionRoleRef]CompactKbuildActionContract{baseRef: contract, ref: contract}
	return plan, node
}

func TestConfigDependencyDriverLinkInitialStateRequiresEquivalentContract(t *testing.T) {
	for _, difference := range []string{"none", "missing base", "prefix", "suffix", "environment"} {
		for _, evidence := range []string{"absent", "positive"} {
			t.Run(difference+"/"+evidence, func(t *testing.T) {
				const source = "lib/crc/helper.c"
				plan, node := configDependencySourceDriverLinkPlanForTest(t, source, map[string]string{
					source: "#define READ(x) CONFIG_ ## x\nint value = $READ(CRC_TABLE);\n",
				}, []string{"-nostdinc"}, false)
				baseRef := KbuildActionRoleRef{Scope: "host", Role: "cc"}
				linkRole, ok := toolaction.LinkContractRole("cc")
				if !ok {
					t.Fatal("cc has no driver-link contract")
				}
				linkRef := KbuildActionRoleRef{Scope: "host", Role: linkRole}
				base := CompactKbuildActionContract{
					PrefixArguments: []string{"-fno-dollars-in-identifiers"},
					SuffixArguments: []string{"-DUNCHANGED=1"},
					Environment:     map[string]string{"LANG": "C"},
				}
				link := CompactKbuildActionContract{
					PrefixArguments: slices.Clone(base.PrefixArguments),
					SuffixArguments: slices.Clone(base.SuffixArguments),
					Environment:     maps.Clone(base.Environment),
				}
				switch difference {
				case "prefix":
					link.PrefixArguments = []string{"-fdollars-in-identifiers"}
				case "suffix":
					link.SuffixArguments = []string{"-DUNCHANGED=2"}
				case "environment":
					link.Environment["LANG"] = "POSIX"
				}
				plan.metadata.actionContracts = map[KbuildActionRoleRef]CompactKbuildActionContract{baseRef: base, linkRef: link}
				if difference == "missing base" {
					delete(plan.metadata.actionContracts, baseRef)
				}
				calls := 0
				if evidence == "positive" {
					plan.metadata.compilerPredefines = func(_, _, _ string, _, _ []string, _ map[string]string) (string, bool, error) {
						calls++
						return "#define UNCHANGED 1\n", true, nil
					}
					plan.metadata.compilerDefinedness = func(_, _, _ string, _, _, names []string, _ map[string]string) (map[string]bool, bool, error) {
						calls++
						result := make(map[string]bool, len(names))
						for _, name := range names {
							result[name] = false
						}
						return result, true, nil
					}
					plan.metadata.compilerDollarPunctuation = func(_, _, _ string, _, _ []string, _ map[string]string) (bool, bool, error) {
						calls++
						return true, true, nil
					}
				}
				invocation, reason := actionPlanConfigDependencyCompilerInvocation(plan, node, plan.Recipes[node.Recipe])
				if reason != "" {
					t.Fatalf("original driver-link invocation must remain inspectable: %s", reason)
				}
				if !slices.Equal(invocation.arguments[:invocation.kbuildStart], link.PrefixArguments) ||
					!slices.Equal(invocation.arguments[invocation.kbuildEnd:], link.SuffixArguments) ||
					invocation.environment["LANG"] != link.Environment["LANG"] {
					t.Fatalf("original link envelope changed: %#v", invocation)
				}
				_, reason = configDependencyCompilerPredefineProbeForInvocation(invocation, []string{source})
				if (reason == "") != (difference == "none") {
					t.Fatalf("projected namespace refusal = %q, contract difference %q", reason, difference)
				}
				if evidence == "positive" {
					// Simulate an already recorded result for the base -E query.
					// It must not authenticate a different original link envelope.
					baseInvocation := invocation
					baseInvocation.predefineContractReason = ""
					probe, reason := configDependencyCompilerPredefineProbeForInvocation(baseInvocation, []string{source})
					if reason != "" {
						t.Fatal(reason)
					}
					key := configDependencyCompilerPredefineKey("host", "cc", probe.language, probe.arguments, probe.translationUnits, probe.environment)
					context := &configDependencyAnalysisContext{
						compilerPredefineRequests: map[configDependencyCompilerPredefineRequestKey]configDependencyCompilerPredefineRequestResult{
							key: {ready: true, lexical: configDependencyCompilerLexicalEvidence{ready: true, dollarPunctuation: true, identity: "recorded-base-contract-proof"}},
						},
					}
					_, ready := configDependencyCompilerLexicalWitness(plan, node, invocation, context)
					if ready != (difference == "none") {
						t.Fatalf("recorded base-contract proof reused across %q: ready=%t", difference, ready)
					}
				}
				set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
				wantPrecise := difference == "none" && evidence == "positive"
				if err != nil || set.Opaque == wantPrecise {
					t.Fatalf("driver-link namespace analysis = %#v, %v", set, err)
				}
				if wantPrecise && (!slices.Equal(set.Symbols, []string{"CONFIG_CRC_TABLE"}) || calls == 0) {
					t.Fatalf("identical measured contracts lost precise CONFIG reads: %#v, calls=%d", set, calls)
				}
				if !wantPrecise && (len(set.Symbols) != 0 || len(set.SourcePaths) != 0 || len(set.ObjectPaths) != 0) {
					t.Fatalf("unproved link namespace published partial proof: %#v", set)
				}
				if difference != "none" && calls != 0 {
					t.Fatalf("incompatible initial-state contract reached %d compiler callbacks", calls)
				}
			})
		}
	}
}

func TestConfigDependencyDriverLinkRetainsQuotedRelativeSourceAndConfigFallback(t *testing.T) {
	const source = "lib/crc/gen_crc32table.c"
	plan, node := configDependencySourceDriverLinkPlanForTest(t, source, map[string]string{
		source:                      "#include <stdio.h>\n#include \"../../include/linux/crc32poly.h\"\n#include \"../../include/generated/autoconf.h\"\nCONFIG_CRC_TABLE\n",
		"include/linux/crc32poly.h": "#define CRC32_POLY_LE 0xedb88320\n",
	}, nil, true)
	before := node
	before.Sources = slices.Clone(node.Sources)
	before.Trees = slices.Clone(node.Trees)
	workingInputs := maps.Clone(plan.Recipes[node.Recipe].WorkingInputs)
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Opaque || set.Reason == "" || len(set.SourcePaths) != 0 {
		t.Fatalf("driver link with uninspectable headers = %#v, want opaque without a partial source projection", set)
	}
	_, changed, err := prunePreciseFamilyCompilerInputs(plan, &node, set)
	if err != nil || changed || !reflect.DeepEqual(node, before) {
		t.Fatalf("opaque driver link was pruned: changed=%t node=%#v error=%v", changed, node, err)
	}
	if !slices.Contains(node.Trees, "kernel") || !maps.Equal(plan.Recipes[node.Recipe].WorkingInputs, workingInputs) {
		t.Fatal("opaque driver link lost full kernel-tree or staged autoconf fallback")
	}
}

func configDependencyUnclassifiedDriverLinkPlanForTest(t *testing.T, persistent bool) *ActionPlan {
	t.Helper()
	// A direct compiler driver can preprocess a source file while linking. It
	// intentionally has no typed CompilerInvocation/compile-node proof here.
	const source = "lib/subdir/program.c"
	recipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "link-driver", Tool: "cc",
		Arguments: []string{
			"-I${work:root}/lib/subdir", "-o", "${output:00000000}",
			"${source:source:00000000}",
		},
		WorkingDirectory: "source-driver-link",
		WorkingInputs:    map[string]string{"source:source:00000000": source},
		Sources:          []string{"source:00000000"},
		Outputs:          []string{"00000000"},
	}
	node := ActionPlanNode{
		ID: "source-driver-link", Stage: "host", Kind: "link-driver", Tool: "cc", Product: "image",
		Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: "src-00000001"}},
		Outputs: []ActionPlanOutput{{Tree: "host", Path: "lib/subdir/program"}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"host": "sha256-" + strings.Repeat("2", 64), "target": "sha256-" + strings.Repeat("1", 64),
		},
		Sources: []ActionPlanSource{
			{ID: "src-00000001", Namespace: "kernel", Path: source},
			{ID: "src-00000002", Namespace: "config", Path: "autoconf.h"},
		},
		Products: []ActionPlanProduct{{Name: "image", Tree: "host", Path: LinuxKernelTreeRootMarker}},
	}
	if persistent {
		// Ambient config is neither an argv use nor a declared CompilerUse.
		// The direct source may nevertheless include it through a quoted path.
		node.InputSet = configDependencyInsertInputSetEntryForTest(t, plan, "", ActionPlanInputSetEntry{
			Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: configDependencyAutoconfPath},
			SourceID: "src-00000002",
		})
	} else {
		recipe.Sources = append(recipe.Sources, "config:00000001")
		recipe.WorkingInputs["source:config:00000001"] = configDependencyAutoconfPath
		node.Sources = append(node.Sources, ActionPlanSourceEdge{Role: "config", SourceID: "src-00000002"})
	}
	recipeID, err := recipe.ID()
	if err != nil {
		t.Fatal(err)
	}
	node.Recipe = recipeID
	plan.Recipes = map[string]ActionRecipe{recipeID: recipe}
	plan.Nodes = []ActionPlanNode{node}
	return plan
}

func TestConfigDependencyUnclassifiedDriverLinkStagedConfigIsOpaque(t *testing.T) {
	for _, binding := range []string{"direct", "persistent"} {
		t.Run(binding, func(t *testing.T) {
			plan := configDependencyUnclassifiedDriverLinkPlanForTest(t, binding == "persistent")
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, plan.Nodes[0])
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || set.Reason == "" || len(set.SourcePaths) != 0 {
				t.Fatalf("source-bearing unclassified driver link = %#v, want opaque full-config fallback", set)
			}
		})
	}
}

func TestActionPlanFamilyUnclassifiedDriverLinkRetainsFullConfig(t *testing.T) {
	configs := map[string]map[string]string{
		"enabled":  familyTestConfig("y", "n"),
		"disabled": familyTestConfig("n", "n"),
	}
	// Use real autoconf Boolean representation, not a y/n-valued C macro.
	configs["enabled"][configDependencyAutoconfPath] = "#define CONFIG_USED 1\n"
	configs["disabled"][configDependencyAutoconfPath] = "\n"
	for _, binding := range []string{"direct", "persistent"} {
		for _, annotation := range []string{"analyzed", "empty"} {
			t.Run(binding+"/"+annotation, func(t *testing.T) {
				var variants []ActionPlanFamilyVariant
				for _, name := range []string{"enabled", "disabled"} {
					plan := configDependencyUnclassifiedDriverLinkPlanForTest(t, binding == "persistent")
					var dependencies ConfigDependencySet
					if annotation == "analyzed" {
						var err error
						dependencies, err = AnalyzeActionPlanNodeConfigDependencies(plan, plan.Nodes[0])
						if err != nil {
							t.Fatal(err)
						}
					}
					// An empty legacy/caller annotation must not bypass the
					// family's independent full-config requirement either.
					if err := contentAddressActionPlanNodes(plan); err != nil {
						t.Fatal(err)
					}
					snapshot, err := canonicalActionPlanSnapshot(plan, map[string]ConfigDependencySet{
						plan.Nodes[0].ID: dependencies,
					}, configs[name])
					if err != nil {
						t.Fatal(err)
					}
					variants = append(variants, ActionPlanFamilyVariant{Name: name, Snapshot: snapshot})
				}
				family, err := BuildActionPlanFamily(variants)
				if err != nil {
					t.Fatal(err)
				}
				if len(family.Nodes) != 2 {
					t.Errorf("changed config driver links coalesced: nodes=%d memberships=%#v", len(family.Nodes), family.Memberships)
				}
				sources := make(map[string]ActionPlanSource, len(family.Sources))
				for _, source := range family.Sources {
					sources[source.ID] = source
				}
				inputSets, err := NewActionPlanInputSetStoreFromNodes(family.InputSets)
				if err != nil {
					t.Fatal(err)
				}
				for _, node := range family.Nodes {
					if node.Kind != "link-driver" || len(family.preciseCompileMemberships) != 0 {
						t.Fatalf("unclassified driver link acquired a compiler precision claim: %#v", node)
					}
					var configSources []ActionPlanSource
					if binding == "persistent" {
						if err := inputSets.Walk(node.InputSet, func(entry ActionPlanInputSetEntry) error {
							if entry.Target.Kind == ActionPlanInputSetWorkTarget && entry.Target.Path == configDependencyAutoconfPath {
								configSources = append(configSources, sources[entry.SourceID])
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					} else {
						for _, edge := range node.Sources {
							if edge.Role == "config" {
								configSources = append(configSources, sources[edge.SourceID])
							}
						}
					}
					if len(configSources) != 1 || configSources[0].Namespace != "capsule" {
						t.Fatalf("localized config bindings = %#v, want one capsule source", configSources)
					}
					digest, projection, ok := strings.Cut(configSources[0].Path, "/")
					if !ok || projection != configDependencyAutoconfPath {
						t.Fatalf("localized autoconf path = %q", configSources[0].Path)
					}
					members := family.Memberships[node.ID]
					if len(members) != 1 {
						t.Errorf("driver link %s memberships = %q, want one changed-config variant", node.ID, members)
					}
					for _, name := range members {
						if got, want := family.Capsules[digest], configs[name]; !maps.Equal(got, want) {
							t.Errorf("%s localized capsule = %#v, want all original config bytes %#v", name, got, want)
						}
					}
				}
			})
		}
	}
}

func TestConfigDependencyDriverLinkUsesFullScannerForLocalClosure(t *testing.T) {
	const source = "lib/crc/helper.c"
	plan, node := configDependencySourceDriverLinkPlanForTest(t, source, map[string]string{
		source:                          "#include \"../../include/linux/local.h\"\n",
		"include/linux/local.h":         "#include \"nested/detail.h\"\nCONFIG_CRC_TABLE\n",
		"include/linux/nested/detail.h": "#define LOCAL_VALUE 1\n",
	}, []string{"-nostdinc"}, false)
	set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"include/linux/local.h", "include/linux/nested/detail.h", source}
	if set.Opaque || !slices.Equal(set.SourcePaths, wantPaths) || !slices.Equal(set.Symbols, []string{"CONFIG_CRC_TABLE"}) {
		t.Fatalf("fully inspectable driver link = %#v, want exact local closure %q and CONFIG_CRC_TABLE", set, wantPaths)
	}
}

func TestConfigDependencyDriverLinkRejectsUnmodeledPreprocessing(t *testing.T) {
	for name, contents := range map[string]string{
		"dynamic include":   "#include SELECTED_HEADER\n",
		"include next":      "#include_next <selected.h>\n",
		"include existence": "#if __has_include(\"selected.h\")\n#endif\n",
		"pragma":            "_Pragma(\"include_alias(\\\"a\\\", \\\"b\\\")\")\n",
		"assembler incbin":  "asm(\".incbin \\\"hidden.bin\\\"\");\n",
	} {
		t.Run(name, func(t *testing.T) {
			const source = "lib/crc/helper.c"
			plan, node := configDependencySourceDriverLinkPlanForTest(t, source, map[string]string{source: contents}, []string{"-nostdinc"}, false)
			set, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
			if err != nil {
				t.Fatal(err)
			}
			if !set.Opaque || strings.TrimSpace(set.Reason) == "" {
				t.Fatalf("unmodeled driver-link preprocessing = %#v, want opaque", set)
			}
		})
	}
}
