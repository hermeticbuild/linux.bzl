package kconfig

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func macroReplacementStateForTest(t *testing.T, text string) *configDependencyMacroState {
	t.Helper()
	state, reason := parseConfigDependencyCompilerPredefinesWithReplacements(text, true)
	if reason != "" {
		t.Fatal(reason)
	}
	return &state
}

func TestConfigDependencyMacroReplacementPredefinesAreOptIn(t *testing.T) {
	text := "#define F(x) CONFIG_ ## x\n#define VALUE 7\n"
	ordinary, reason := parseConfigDependencyCompilerPredefines(text)
	if reason != "" || ordinary.macroReplacements != nil {
		t.Fatalf("ordinary scanner retained replacements: %q", reason)
	}
	exact := macroReplacementStateForTest(t, text)
	if got := exact.macroReplacements["F"]; got.text != "F(x) CONFIG_ ## x" || got.origin == "" {
		t.Fatalf("lost lazy function definition: %#v", got)
	}
	other := macroReplacementStateForTest(t, text)
	if !maps.Equal(exact.macroReplacements, other.macroReplacements) {
		t.Fatal("identical compiler dumps have unstable identities")
	}
	if exact.definition("__UNQUERIED") != configDependencyMacroUnknown {
		t.Fatal("-dM was treated as a complete initial macro namespace")
	}
}

func TestConfigDependencyMacroReplacementBranchesOwnTheirValues(t *testing.T) {
	root := macroReplacementStateForTest(t, "#define VALUE 1\n")
	original := root.macroReplacements["VALUE"]
	left, right := root.branch(), root.branch()
	if !left.setSourceMacroReplacement("VALUE 2", "source:left:1", configDependencyMacroDefined) {
		t.Fatal("definition rejected")
	}
	if root.macroReplacements["VALUE"] != original || right.macroReplacements["VALUE"] != original {
		t.Fatal("branch mutated shared replacement state")
	}
	root.mergePossibleBranches(left, right)
	if root.tainted || root.definition("VALUE") != configDependencyMacroDefined {
		t.Fatal("differing bodies must retain common definedness")
	}
	if _, modeled := root.macroReplacements["VALUE"]; modeled {
		t.Fatal("branch-dependent replacement retained as exact")
	}
	if !left.consumed || !right.consumed {
		t.Fatal("joined children not consumed")
	}
}

func TestConfigDependencyMacroReplacementCommitAndConsume(t *testing.T) {
	root := macroReplacementStateForTest(t, "#define VALUE 1\n")
	child := root.branch()
	child.setSourceMacroReplacement("VALUE 2", "source:1", configDependencyMacroDefined)
	if !root.commitBranch(child) || root.macroReplacements["VALUE"].text != "VALUE 2" {
		t.Fatal("commit lost replacement")
	}
	child.set("VALUE", configDependencyMacroUndefined)
	if root.macroReplacements["VALUE"].text != "VALUE 2" || child.macroReplacements != nil {
		t.Fatal("consumed child mutated adopted replacement map")
	}
}

func TestConfigDependencyMacroReplacementOnlyChangeInvalidatesSibling(t *testing.T) {
	for _, join := range []bool{false, true} {
		root := macroReplacementStateForTest(t, "#define VALUE 1\n")
		changed, stale := root.branch(), root.branch()
		changed.setSourceMacroReplacement("VALUE 2", "source:1", configDependencyMacroDefined)
		revision := root.snapshotRevision
		if join {
			root.mergePossibleBranch(changed)
		} else if !root.commitBranch(changed) {
			t.Fatal("exact commit rejected")
		}
		if root.snapshotRevision == revision || stale.validSnapshotLineage() {
			t.Fatalf("replacement-only change left sibling valid: join=%t", join)
		}
		before := maps.Clone(root.macroReplacements)
		if root.commitBranch(stale) || !maps.Equal(before, root.macroReplacements) {
			t.Fatalf("stale sibling restored an old body: join=%t", join)
		}
	}
}

func TestConfigDependencyMacroReplacementUnknownWrite(t *testing.T) {
	for _, identical := range []bool{false, true} {
		state := macroReplacementStateForTest(t, "")
		state.setSourceMacroReplacement("VALUE 1", "source:1", configDependencyMacroDefined)
		text := "VALUE 2"
		if identical {
			text = "VALUE 1"
		}
		state.setSourceMacroReplacement(text, "source:1", configDependencyMacroUnknown)
		_, modeled := state.macroReplacements["VALUE"]
		if modeled != identical || state.definition("VALUE") != configDependencyMacroDefined {
			t.Fatalf("unknown write: identical=%t modeled=%t", identical, modeled)
		}
	}
}

func TestConfigDependencyMacroReplacementUndefinedAndUnmodeledWrites(t *testing.T) {
	for _, value := range []configDependencyMacroDefinition{configDependencyMacroUnknown, configDependencyMacroUndefined, configDependencyMacroDefined} {
		state := macroReplacementStateForTest(t, "#define VALUE 1\n")
		state.set("VALUE", value)
		if _, modeled := state.macroReplacements["VALUE"]; modeled {
			t.Fatalf("definedness-only write %d retained stale body", value)
		}
	}
}

func TestConfigDependencyMacroReplacementNumericHeaderDropsUnknownValues(t *testing.T) {
	state := macroReplacementStateForTest(t, "#define VALUE 1\n#define CONFIG_KEEP 7\n")
	state.applyValidatedNumericMacroHeader()
	if state.definition("VALUE") != configDependencyMacroDefined || state.macroReplacements["VALUE"].text != "" {
		t.Fatal("numeric header retained a possibly overwritten body")
	}
	if state.macroReplacements["CONFIG_KEEP"].text != "CONFIG_KEEP 7" {
		t.Fatal("numeric header invalidated a CONFIG name it cannot write")
	}
}

func TestConfigDependencyMacroReplacementAutoconfValuesAndGuard(t *testing.T) {
	state := macroReplacementStateForTest(t, "#define CONFIG_OMITTED 9\n")
	definitions := newConfigDependencyResolvedAutoconfDefinitions(map[string]string{
		"CONFIG_Y": "y", "CONFIG_M": "m", "CONFIG_N": "n", "CONFIG_NUMBER": "0x20", "CONFIG_STRING": `"text"`,
	})
	state.applyResolvedConfigAutoconf(definitions)
	for name, expected := range map[string]string{
		"CONFIG_Y": "CONFIG_Y 1", "CONFIG_M_MODULE": "CONFIG_M_MODULE 1", "CONFIG_NUMBER": "CONFIG_NUMBER 0x20",
		"CONFIG_STRING": `CONFIG_STRING "text"`, "CONFIG_OMITTED": "CONFIG_OMITTED 9",
	} {
		if state.macroReplacements[name].text != expected {
			t.Errorf("%s = %#v, want %s", name, state.macroReplacements[name], expected)
		}
	}
	if _, present := state.macroReplacements["CONFIG_N"]; present {
		t.Fatal("n-valued config emitted a definition")
	}
	state.applyResolvedConfigAutoconf(newConfigDependencyResolvedAutoconfDefinitions(map[string]string{"CONFIG_NUMBER": "8"}))
	if state.macroReplacements["CONFIG_NUMBER"].text != "CONFIG_NUMBER 0x20" {
		t.Fatal("already-defined guard failed to skip the autoconf body")
	}
}

func TestConfigDependencyMacroReplacementUnknownAutoconfGuard(t *testing.T) {
	state := macroReplacementStateForTest(t, "#define CONFIG_VALUE 9\n")
	state.set(configDependencyResolvedAutoconfGuard, configDependencyMacroUnknown)
	state.applyResolvedConfigAutoconf(newConfigDependencyResolvedAutoconfDefinitions(map[string]string{"CONFIG_VALUE": "7"}))
	if state.definition("CONFIG_VALUE") != configDependencyMacroDefined {
		t.Fatal("common config definedness lost")
	}
	if _, modeled := state.macroReplacements["CONFIG_VALUE"]; modeled {
		t.Fatal("unknown guard selected one of two replacement values")
	}
	if _, modeled := state.macroReplacements[configDependencyResolvedAutoconfGuard]; modeled {
		t.Fatal("unknown guard's previous body was assumed empty")
	}
}

func TestConfigDependencyMacroReplacementDisablesDefinednessCaches(t *testing.T) {
	state := macroReplacementStateForTest(t, "#define VALUE 1\n")
	if configDependencyMacroEffectStateReady(state) || state.beginForcedHeaderTrace() {
		t.Fatal("replacement-sensitive state admitted into a definedness-only cache")
	}
	child := state.branch()
	if _, valid := captureConfigDependencyMacroEffect(child); valid {
		t.Fatal("captured an effect without replacement reads/writes")
	}
}

func TestConfigDependencyMacroReplacementOrderedSourceSpans(t *testing.T) {
	state := macroReplacementStateForTest(t, "")
	source := "#define PICK(x) CONFIG_ ## x\nPICK\n(ONE)\n#undef PICK\n#define PICK(x) CONFIG_ ## x ## _MODULE\nPICK(TWO)\n"
	program := compileConfigDependencyConditionalProgramWithCalls([]byte(source), "immutable-source:test", true)
	var spans, definitions []string
	_, reason := interpretConfigDependencyConditionalProgramWithText(program, state, nil, "c", func(text string, current *configDependencyMacroState) string {
		if strings.TrimSpace(text) != "" {
			spans = append(spans, text)
			definitions = append(definitions, current.macroReplacements["PICK"].text)
		}
		return ""
	})
	if reason != "" {
		t.Fatal(reason)
	}
	if !slices.Equal(spans, []string{"PICK\n(ONE)\n", "PICK(TWO)\n\n"}) ||
		!slices.Equal(definitions, []string{"PICK(x) CONFIG_ ## x", "PICK(x) CONFIG_ ## x ## _MODULE"}) {
		t.Fatalf("source spans or ordered definitions lost: %#v %#v", spans, definitions)
	}
}

func TestConfigDependencyMacroReplacementCoverageRejectsPartialSource(t *testing.T) {
	for name, source := range map[string]string{
		"unknown_condition":  "#if __UNQUERIED\n#define F(x) CONFIG_ ## x\n#endif\n",
		"unknown_guard":      "#ifdef __UNQUERIED\n#endif\n",
		"unresolved_include": "#include <unknown.h>\n",
		"pragma":             "#pragma vendor_effect\n",
		"line":               "#line 99\n",
	} {
		t.Run(name, func(t *testing.T) {
			state := macroReplacementStateForTest(t, "")
			program := compileConfigDependencyConditionalProgramWithCalls([]byte(source), "immutable-source:"+name, true)
			_, reason := interpretConfigDependencyConditionalProgramWithText(program, state, nil, "c", func(string, *configDependencyMacroState) string { return "" })
			if reason == "" {
				t.Fatal("accepted incomplete call coverage")
			}
		})
	}
}

func TestConfigDependencyMacroReplacementCoverageSkipsInactiveUnsupportedEffects(t *testing.T) {
	state := macroReplacementStateForTest(t, "")
	program := compileConfigDependencyConditionalProgramWithCalls([]byte("#if 0\n#if UNKNOWN\n#pragma effect\n#include <missing.h>\nBAD_TEXT\n#endif\n#endif\n"), "immutable-source:inactive", true)
	_, reason := interpretConfigDependencyConditionalProgramWithText(program, state, nil, "c", func(text string, _ *configDependencyMacroState) string {
		if strings.TrimSpace(text) != "" {
			t.Fatalf("inactive text was expanded: %q", text)
		}
		return ""
	})
	if reason != "" {
		t.Fatal(reason)
	}
}

func TestConfigDependencyMacroReplacementCoverageRequiresRetainedSpans(t *testing.T) {
	state := macroReplacementStateForTest(t, "")
	program := compileConfigDependencyConditionalProgram([]byte("UNMODELED_CALL(value)\n"))
	_, reason := interpretConfigDependencyConditionalProgramWithText(program, state, nil, "c", func(string, *configDependencyMacroState) string { return "" })
	if reason == "" {
		t.Fatal("ordinary syntax-only program was accepted as complete text coverage")
	}
}
