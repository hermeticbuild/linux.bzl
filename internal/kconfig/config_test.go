package kconfig

import (
	"context"
	"maps"
	"strings"
	"testing"
)

func TestResolveConfigTransitiveSelect(t *testing.T) {
	resolved := mustResolveConfig(t, `
mainmenu "Test"

config SELECTOR
	bool "Selector"
	select MID

config MID
	bool "Middle"
	select TARGET

config TARGET
	bool "Target"
`, map[string]string{
		"CONFIG_SELECTOR": "y",
	})

	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_SELECTOR": "y",
		"CONFIG_MID":      "y",
		"CONFIG_TARGET":   "y",
	})
}

func TestResolveConfigUsesMeasuredDefaultsForHiddenSymbols(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
config MEASURED_VERSION
	int
	default $(MEASURED_VERSION)

config MEASURED_CAPABILITY
	bool
	default $(MEASURED_CAPABILITY)

config USER_CHOICE
	bool "User choice"
	default n
`), "Kconfig", Options{Variables: map[string]string{
		"MEASURED_VERSION":    "109800",
		"MEASURED_CAPABILITY": "y",
	}})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	resolved, err := tree.ResolveConfig(map[string]string{
		"CONFIG_MEASURED_VERSION":    "107800",
		"CONFIG_MEASURED_CAPABILITY": "n",
		"CONFIG_USER_CHOICE":         "y",
	})
	if err != nil {
		t.Fatalf("ResolveConfig() failed: %v", err)
	}
	if got := resolved.Value("CONFIG_MEASURED_VERSION"); got != "109800" {
		t.Fatalf("CONFIG_MEASURED_VERSION = %q, want measured default", got)
	}
	if got := resolved.Value("CONFIG_MEASURED_CAPABILITY"); got != "y" {
		t.Fatalf("CONFIG_MEASURED_CAPABILITY = %q, want measured default", got)
	}
	if got := resolved.Value("CONFIG_USER_CHOICE"); got != "y" {
		t.Fatalf("CONFIG_USER_CHOICE = %q, want imported visible value", got)
	}
}

func TestCompactMetadataWithOptionsStoresOneResolvedFragment(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
config ENABLED
	bool "Enabled"

config HIDDEN
	def_bool y
`), "Kconfig", Options{})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	metadata, err := tree.CompactMetadataWithOptions(
		map[string]string{"CONFIG_ENABLED": "y"},
		ResolveConfigOptions{},
		CompactMetadataOptions{SelectedProductsOnly: true},
		func(resolved *ResolvedConfig) (CompactConfigGraph, error) {
			if resolved.Value("CONFIG_ENABLED") != "y" || resolved.Value("CONFIG_HIDDEN") != "y" {
				t.Fatalf("graph resolver received unresolved config: %#v", resolved.Effective)
			}
			return CompactConfigGraph{ImageTarget: "image"}, nil
		},
	)
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}
	if got, want := metadata.configFragment, map[string]string{
		"CONFIG_ENABLED": "y",
		"CONFIG_HIDDEN":  "y",
	}; !maps.Equal(got, want) {
		t.Fatalf("resolved fragment = %#v, want %#v", got, want)
	}
	if metadata.Config.imageTarget != "image" {
		t.Fatalf("image target = %q, want image", metadata.Config.imageTarget)
	}
}

func TestCompactMetadataForResolvedConfigWithOptionsReusesResolvedValue(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
config ENABLED
	bool "Enabled"
`), "Kconfig", Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := tree.ResolveConfig(map[string]string{"CONFIG_ENABLED": "y"})
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	metadata, err := tree.CompactMetadataForResolvedConfigWithOptions(
		resolved,
		CompactMetadataOptions{SelectedProductsOnly: true},
		func(got *ResolvedConfig) (CompactConfigGraph, error) {
			called++
			if got != resolved {
				t.Fatal("resolved-config phase boundary cloned or re-resolved its input")
			}
			return CompactConfigGraph{ImageTarget: "image"}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 || metadata.configFragment["CONFIG_ENABLED"] != "y" {
		t.Fatalf("resolved metadata callback calls=%d fragment=%#v", called, metadata.configFragment)
	}
	if resolved.Effective["CONFIG_ENABLED"] != "y" {
		t.Fatal("resolved config was mutated")
	}
	if _, err := tree.CompactMetadataForResolvedConfigWithOptions(
		nil, CompactMetadataOptions{}, func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{}, nil
		},
	); err == nil || !strings.Contains(err.Error(), "must not be nil") {
		t.Fatalf("nil resolved config error = %v", err)
	}
}

func TestCompactMetadataFirstLoweringConsumesValidatedSelectionGraph(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
config ENABLED
	bool "Enabled"
`), "Kconfig", Options{})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := tree.CompactMetadataWithOptions(
		map[string]string{"CONFIG_ENABLED": "y"},
		ResolveConfigOptions{},
		CompactMetadataOptions{PreconfiguredObjectTree: true, SelectedProductsOnly: true},
		func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{ImageTarget: "image"}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	validated := metadata.validatedSelectionGraph
	if validated == nil {
		t.Fatal("metadata construction did not retain its validated selection graph")
	}

	_, first, err := metadata.lowerSelectedActionPlan(
		actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, false, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first != validated {
		t.Fatal("first lowering rebuilt the selection graph validated by metadata construction")
	}
	if metadata.validatedSelectionGraph != nil {
		t.Fatal("first lowering retained a mutable selection graph for reuse")
	}

	_, second, err := metadata.lowerSelectedActionPlan(
		actionPlanTestProbeIdentity, actionPlanTestProbeIdentity, false, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("later lowering reused materialization state from the first traversal")
	}
}

func TestCompactMetadataRetainedSelectionGraphStillValidatesEagerly(t *testing.T) {
	tree, err := Parse(context.Background(), strings.NewReader(`
config ENABLED
	bool "Enabled"
`), "Kconfig", Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tree.CompactMetadataWithOptions(
		map[string]string{"CONFIG_ENABLED": "y"},
		ResolveConfigOptions{},
		CompactMetadataOptions{SelectedProductsOnly: true},
		func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{
				KbuildSelections: []CompactKbuildSelection{{
					Profile: "missing", Target: "target", MakeTarget: "target",
					Lifecycle: "target", Scope: "target", Stage: "target",
				}},
			}, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), `resolve Kbuild selections: Kbuild selection references missing profile "missing"`) {
		t.Fatalf("metadata construction error = %v, want eager selection-graph validation", err)
	}
}

func TestParseConfigPreservesCanonicalUnsetComments(t *testing.T) {
	raw, err := ParseConfig(strings.NewReader(`# Generated configuration
# CONFIG_DEFAULT_ON is not set
# CONFIG_ is not set
# CONFIG_SPACED  is not set
# CONFIG_TRAILING is not set trailing text
# CONFIG_DIFFERENT_COMMENT has another meaning
`))
	if err != nil {
		t.Fatalf("ParseConfig() failed: %v", err)
	}
	if got, want := raw, map[string]string{"CONFIG_DEFAULT_ON": "n"}; !maps.Equal(got, want) {
		t.Fatalf("ParseConfig() = %#v, want %#v", got, want)
	}
	resolved := mustResolveConfig(t, `
mainmenu "Test"

config DEFAULT_ON
	bool "Default on"
	default y
	`, raw)

	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_DEFAULT_ON": "n",
	})
}

func TestParseConfigRejectsDuplicateAssignmentAndUnset(t *testing.T) {
	_, err := ParseConfig(strings.NewReader("CONFIG_DUPLICATE=y\n# CONFIG_DUPLICATE is not set\n"))
	if err == nil || !strings.Contains(err.Error(), `duplicate config key "CONFIG_DUPLICATE"`) {
		t.Fatalf("ParseConfig() error = %v, want duplicate key", err)
	}
}

func TestResolveConfigAllNoConfigStartsFromN(t *testing.T) {
	fixture := `
mainmenu "Test"

config DEFAULT_ON
	bool "Default on"
	default y

config GATE
	bool "Gate"
	default y

config DEP_DEFAULT
	bool "Depends default"
	depends on GATE
	default y

config SELECTOR
	bool "Selector"
	select SELECTED

config SELECTED
	bool "Selected"

config HIDDEN_DEFAULT
	def_bool y

config HIDDEN_PROMPT_DEFAULT
	bool "Hidden prompt default" if GATE
	default y
`
	resolved := mustResolveConfigWithOptions(t, fixture, nil, ResolveConfigOptions{
		AllNoConfig: true,
	})
	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_DEFAULT_ON":            "n",
		"CONFIG_DEP_DEFAULT":           "n",
		"CONFIG_GATE":                  "n",
		"CONFIG_HIDDEN_DEFAULT":        "y",
		"CONFIG_HIDDEN_PROMPT_DEFAULT": "y",
		"CONFIG_SELECTED":              "n",
		"CONFIG_SELECTOR":              "n",
	})

	explicit := mustResolveConfigWithOptions(t, fixture, map[string]string{
		"CONFIG_SELECTOR": "y",
	}, ResolveConfigOptions{
		AllNoConfig: true,
	})
	wantConfigValues(t, explicit, map[string]string{
		"CONFIG_DEFAULT_ON":            "n",
		"CONFIG_DEP_DEFAULT":           "n",
		"CONFIG_GATE":                  "n",
		"CONFIG_HIDDEN_DEFAULT":        "y",
		"CONFIG_HIDDEN_PROMPT_DEFAULT": "y",
		"CONFIG_SELECTED":              "y",
		"CONFIG_SELECTOR":              "y",
	})
}

func TestResolveConfigSelectBypassesTargetDepends(t *testing.T) {
	fixture := `
mainmenu "Test"

config GATE
	bool "Gate"

config SELECTOR
	bool "Selector"
	select TARGET

config TARGET
	tristate "Target"
	depends on GATE
`
	t.Run("blocked", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_SELECTOR": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_SELECTOR": "y",
			"CONFIG_TARGET":   "y",
		})
	})

	t.Run("allowed", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_GATE":     "y",
			"CONFIG_SELECTOR": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_GATE":     "y",
			"CONFIG_SELECTOR": "y",
			"CONFIG_TARGET":   "y",
		})
	})
}

// A symbol may be defined in several places. dir_dep is the OR of every
// definition's inherited menu dependency (see finalizeMenu), so a symbol with
// one definition inside a dead "if" block and another that applies carries the
// dead block's condition in dir_dep. scripts/kconfig/symbol.c:sym_calc_value()
// gates the user value and the default on the prompt/default condition, and
// uses dir_dep only to bound "imply", so the live definition still wins.
//
// arch/Kconfig defines CPU_MITIGATIONS inside "if
// !ARCH_CONFIGURES_CPU_MITIGATIONS" while arch/x86/Kconfig defines it as a
// plain "menuconfig ... default y"; x86 selects ARCH_CONFIGURES_CPU_MITIGATIONS,
// so clamping by dir_dep disabled CPU_MITIGATIONS and every MITIGATION_* symbol
// guarded by it.
func TestResolveConfigRedefinedSymbolIgnoresDeadDefinitionDependency(t *testing.T) {
	fixture := `
mainmenu "Test"

config ARCH_CONFIGURES_IT
	bool

config ARCH
	bool "Arch"
	default y
	select ARCH_CONFIGURES_IT

if !ARCH_CONFIGURES_IT

config FEATURE
	def_bool y

endif

config FEATURE
	bool "Feature"
	default y

if FEATURE

config FEATURE_CHILD
	bool "Child"
	default y

endif
`

	t.Run("default", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_ARCH_CONFIGURES_IT": "y",
			"CONFIG_FEATURE":            "y",
			"CONFIG_FEATURE_CHILD":      "y",
		})
	})

	t.Run("explicit user value", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_FEATURE": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_FEATURE":       "y",
			"CONFIG_FEATURE_CHILD": "y",
		})
	})

	t.Run("explicit user n still wins", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_FEATURE": "n",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_FEATURE":       "n",
			"CONFIG_FEATURE_CHILD": "n",
		})
	})
}

// An automatic submenu is inferred when a symbol's prompt depends on the
// preceding symbol. That inferred display hierarchy must not behave like an
// explicit enclosing `if`: the child's defaults remain governed by their own
// conditions even when the prompt and inferred parent are hidden.
//
// arch/powerpc/Kconfig uses this shape for DATA_SHIFT. DATA_SHIFT_BOOL has
// 32-bit-only dependencies, while DATA_SHIFT has an unconditional PPC64
// default that is consumed by the linker script.
func TestResolveConfigAutomaticSubmenuDoesNotHideScalarDefaults(t *testing.T) {
	resolved := mustResolveConfig(t, `
mainmenu "Test"

config ADVANCED_OPTIONS
	bool

config STRICT_KERNEL_RWX
	bool
	default y

config PPC64
	bool
	default y

config DATA_SHIFT_BOOL
	bool "Set custom data alignment"
	depends on ADVANCED_OPTIONS
	depends on STRICT_KERNEL_RWX

config PAGE_SHIFT
	int
	default 16

config DATA_SHIFT
	int "Data shift" if DATA_SHIFT_BOOL
	default 24 if STRICT_KERNEL_RWX && PPC64
	default PAGE_SHIFT
`, nil)

	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_DATA_SHIFT_BOOL": "n",
		"CONFIG_DATA_SHIFT":      "24",
	})
	wantConfigWriteSet(t, resolved, map[string]bool{
		"CONFIG_DATA_SHIFT": true,
	})
}

// The counterpart to the above: a symbol whose only definition sits in a dead
// "if" block has no live definition, so it must stay disabled even when the
// imported config asks for it.
func TestResolveConfigSoleDefinitionInDeadIfStaysDisabled(t *testing.T) {
	resolved := mustResolveConfig(t, `
mainmenu "Test"

config GATE
	bool "Gate"

if GATE

config TARGET
	bool "Target"
	default y

endif
`, map[string]string{
		"CONFIG_TARGET": "y",
	})

	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_GATE":   "n",
		"CONFIG_TARGET": "n",
	})
}

func TestResolveConfigRawValueConstrainedByTargetDepends(t *testing.T) {
	resolved := mustResolveConfig(t, `
mainmenu "Test"

config GATE
	bool "Gate"

config TARGET
	bool "Target"
	depends on GATE
`, map[string]string{
		"CONFIG_TARGET": "y",
	})

	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_GATE":   "n",
		"CONFIG_TARGET": "n",
	})
}

func TestResolveConfigImplyConstrainedByDepends(t *testing.T) {
	fixture := `
mainmenu "Test"

config GATE
	bool "Gate"

config PROMPT
	bool "Prompt"

config SOURCE
	bool "Source"
	imply TARGET

config TARGET
	tristate "Target" if PROMPT
	depends on GATE
`
	t.Run("blocked", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_SOURCE": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_SOURCE": "y",
			"CONFIG_TARGET": "n",
		})
	})

	t.Run("allowed", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_GATE":   "y",
			"CONFIG_SOURCE": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_GATE":   "y",
			"CONFIG_SOURCE": "y",
			"CONFIG_TARGET": "y",
		})
	})

	t.Run("visible user value overrides imply", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_GATE":   "y",
			"CONFIG_PROMPT": "y",
			"CONFIG_SOURCE": "y",
			"CONFIG_TARGET": "n",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_SOURCE": "y",
			"CONFIG_TARGET": "n",
		})
	})

	t.Run("hidden user value does not override imply", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_GATE":   "y",
			"CONFIG_SOURCE": "y",
			"CONFIG_TARGET": "n",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_SOURCE": "y",
			"CONFIG_TARGET": "y",
		})
	})
}

func TestResolveConfigDefaultsGatedByDependsAndIf(t *testing.T) {
	fixture := `
mainmenu "Test"

config GATE
	bool "Gate"

config COND
	bool "Condition"

config DEP_DEFAULT
	bool "Depends default"
	depends on GATE
	default y

config IF_DEFAULT
	bool "If default"
	default y if COND

config FIRST_VISIBLE_DEFAULT
	bool "First visible default"
	default n if COND
	default y
`
	t.Run("enabled", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_COND": "y",
			"CONFIG_GATE": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_DEP_DEFAULT":           "y",
			"CONFIG_IF_DEFAULT":            "y",
			"CONFIG_FIRST_VISIBLE_DEFAULT": "n",
		})
	})

	t.Run("if blocked with fallback", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_GATE": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_DEP_DEFAULT":           "y",
			"CONFIG_IF_DEFAULT":            "n",
			"CONFIG_FIRST_VISIBLE_DEFAULT": "y",
		})
	})

	t.Run("depends blocked", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, nil)
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_DEP_DEFAULT": "n",
		})
	})
}

func TestResolveConfigBoolAndTristateClamping(t *testing.T) {
	resolved := mustResolveConfig(t, `
mainmenu "Test"

config ENABLE_MODULES
	bool "Enable modules"
	modules

config GATE
	tristate "Gate"

config BOOL_ON_MODULES
	bool "Bool on modules"
	depends on GATE

config TRISTATE_ON_MODULES
	tristate "Tristate on modules"
	depends on GATE

config BOOL_DEFAULT_M
	bool "Bool default m"
	default m
`, map[string]string{
		"CONFIG_ENABLE_MODULES":      "y",
		"CONFIG_GATE":                "m",
		"CONFIG_BOOL_ON_MODULES":     "m",
		"CONFIG_TRISTATE_ON_MODULES": "y",
	})

	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_BOOL_DEFAULT_M":      "y",
		"CONFIG_BOOL_ON_MODULES":     "y",
		"CONFIG_ENABLE_MODULES":      "y",
		"CONFIG_GATE":                "m",
		"CONFIG_TRISTATE_ON_MODULES": "m",
	})
}

func TestResolveConfigModulesOptionControlsTristateM(t *testing.T) {
	fixture := `
mainmenu "Test"

config MODULES
	bool "Enable modules"
	modules

config DEFAULT_M
	tristate "Default m"
	default m

config RAW_M
	tristate "Raw m"

config DEPENDS_ON_M
	tristate "Depends on m"
	depends on m
	default y

config DEPENDS_ON_MODULES
	tristate "Depends on modules"
	depends on MODULES
	default m
`
	t.Run("modules disabled", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_RAW_M": "m",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_DEFAULT_M":          "y",
			"CONFIG_DEPENDS_ON_M":       "n",
			"CONFIG_DEPENDS_ON_MODULES": "n",
			"CONFIG_MODULES":            "n",
			"CONFIG_RAW_M":              "y",
		})
	})

	t.Run("modules enabled", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_MODULES": "y",
			"CONFIG_RAW_M":   "m",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_DEFAULT_M":          "m",
			"CONFIG_DEPENDS_ON_M":       "m",
			"CONFIG_DEPENDS_ON_MODULES": "m",
			"CONFIG_MODULES":            "y",
			"CONFIG_RAW_M":              "m",
		})
	})
}

func TestResolveConfigRawValueRequiresPromptVisibility(t *testing.T) {
	fixture := `
mainmenu "Test"

config GATE
	bool "Gate"

config PROMPT_HIDDEN
	bool "Prompt hidden" if GATE

config DEFAULTED_HIDDEN
	bool "Defaulted hidden" if GATE
	default y

config MODE
	string "Mode" if GATE
	default "auto"

config COUNT
	int

config ADDRESS
	hex
`
	t.Run("hidden", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_DEFAULTED_HIDDEN": "n",
			"CONFIG_MODE":             `"manual"`,
			"CONFIG_PROMPT_HIDDEN":    "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_ADDRESS":          "0x0",
			"CONFIG_COUNT":            "0",
			"CONFIG_DEFAULTED_HIDDEN": "y",
			"CONFIG_MODE":             `"auto"`,
			"CONFIG_PROMPT_HIDDEN":    "n",
		})
	})

	t.Run("visible", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_GATE":          "y",
			"CONFIG_MODE":          `"manual"`,
			"CONFIG_PROMPT_HIDDEN": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_MODE":          `"manual"`,
			"CONFIG_PROMPT_HIDDEN": "y",
		})
	})
}

func TestResolveConfigWritesEmptyVisibleString(t *testing.T) {
	resolved := mustResolveConfig(t, `
config CMDLINE
	string "Built-in kernel command line"
`, nil)

	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_CMDLINE": `""`,
	})
	wantConfigWriteSet(t, resolved, map[string]bool{
		"CONFIG_CMDLINE": true,
	})
}

func TestResolveConfigScalarRanges(t *testing.T) {
	fixture := `
mainmenu "Test"

config SMALL_RANGE
	bool "Small range"

config MIN_BOUND
	int "Minimum"
	default 4

config MAX_BOUND
	int "Maximum"
	default 8

config CLAMPED_INT
	int "Clamped int"
	range MIN_BOUND MAX_BOUND

config DEFAULT_LOW
	int "Default low"
	default 1
	range 3 9

config SELECTED_RANGE
	int "Selected range"
	range 0 10 if SMALL_RANGE
	range 20 30

config HEX_VALUE
	hex "Hex value"
	range 0x10 0x20

config LARGE_HEX_DEFAULT
	hex
	default 0xdead000000000000

config LARGE_HEX_RANGE
	hex "Large hex range"
	range 0x8000000000000000 0xffffffffffffffff
`
	t.Run("raw values clamp to active ranges", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_CLAMPED_INT":     "100",
			"CONFIG_HEX_VALUE":       "30",
			"CONFIG_LARGE_HEX_RANGE": "0x7000000000000000",
			"CONFIG_SELECTED_RANGE":  "15",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_CLAMPED_INT":       "8",
			"CONFIG_DEFAULT_LOW":       "3",
			"CONFIG_HEX_VALUE":         "0x20",
			"CONFIG_LARGE_HEX_DEFAULT": "0xdead000000000000",
			"CONFIG_LARGE_HEX_RANGE":   "0x8000000000000000",
			"CONFIG_SELECTED_RANGE":    "20",
		})
	})

	t.Run("first visible range wins", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_SELECTED_RANGE": "15",
			"CONFIG_SMALL_RANGE":    "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_SELECTED_RANGE": "10",
		})
	})

	t.Run("invalid raw values fall back to scalar defaults", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_CLAMPED_INT": "09",
			"CONFIG_HEX_VALUE":   "not-hex",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_CLAMPED_INT": "4",
			"CONFIG_HEX_VALUE":   "0x10",
		})
	})
}

func TestResolveConfigScalarWriteSet(t *testing.T) {
	resolved := mustResolveConfig(t, `
mainmenu "Test"

config HIDDEN_INT
	int

config PROMPT_INT
	int "Prompt integer"

config GATE
	bool "Gate"

config DEP_DEFAULT
	int "Dependent default"
	depends on GATE
	default 7

config DEFAULT_ZERO
	int
	default HIDDEN_INT if HIDDEN_INT = 0
`, nil)

	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_DEFAULT_ZERO": "0",
		"CONFIG_DEP_DEFAULT":  "0",
		"CONFIG_HIDDEN_INT":   "0",
		"CONFIG_PROMPT_INT":   "0",
	})
	wantConfigWriteSet(t, resolved, map[string]bool{
		"CONFIG_DEFAULT_ZERO": true,
		"CONFIG_DEP_DEFAULT":  false,
		"CONFIG_HIDDEN_INT":   false,
		"CONFIG_PROMPT_INT":   true,
	})
}

func TestResolveConfigMenuVisibleIfOnlyHidesPrompts(t *testing.T) {
	fixture := `
mainmenu "Test"

config SHOW_MENU
	bool "Show menu"

menu "Advanced"
	visible if SHOW_MENU

config RAW_VALUE
	bool "Raw value"

config DEFAULTED
	bool "Defaulted"
	default y

config SELECTOR
	bool "Selector"
	default y
	select TARGET

config TARGET
	bool

endmenu
`
	t.Run("hidden prompts", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_RAW_VALUE": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_DEFAULTED": "y",
			"CONFIG_RAW_VALUE": "n",
			"CONFIG_SELECTOR":  "y",
			"CONFIG_TARGET":    "y",
		})
	})

	t.Run("visible prompts", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_RAW_VALUE": "y",
			"CONFIG_SHOW_MENU": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_RAW_VALUE": "y",
		})
	})
}

func TestResolveConfigChoiceDefaultsAndSingleSelection(t *testing.T) {
	fixture := `
mainmenu "Test"

config USE_SECOND
	bool "Use second"

config THIRD_VISIBLE
	bool "Third visible"

choice
	prompt "Backend"
	default SECOND if USE_SECOND
	default FIRST

config FIRST
	bool "First"

config SECOND
	bool "Second"

config THIRD
	bool "Third"
	depends on THIRD_VISIBLE

endchoice
`
	t.Run("fallback default", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, nil)
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_FIRST":  "y",
			"CONFIG_SECOND": "n",
			"CONFIG_THIRD":  "n",
		})
	})

	t.Run("conditional default", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_USE_SECOND": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_FIRST":  "n",
			"CONFIG_SECOND": "y",
			"CONFIG_THIRD":  "n",
		})
	})

	t.Run("explicit visible value", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_SECOND": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_FIRST":  "n",
			"CONFIG_SECOND": "y",
			"CONFIG_THIRD":  "n",
		})
	})

	t.Run("hidden explicit value falls back", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_THIRD": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_FIRST":  "y",
			"CONFIG_SECOND": "n",
			"CONFIG_THIRD":  "n",
		})
	})

	t.Run("explicit n vetoes default", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_FIRST": "n",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_FIRST":  "n",
			"CONFIG_SECOND": "y",
			"CONFIG_THIRD":  "n",
		})
	})

	t.Run("hidden member becomes selectable", func(t *testing.T) {
		resolved := mustResolveConfig(t, fixture, map[string]string{
			"CONFIG_THIRD":         "y",
			"CONFIG_THIRD_VISIBLE": "y",
		})
		wantConfigValues(t, resolved, map[string]string{
			"CONFIG_FIRST":  "n",
			"CONFIG_SECOND": "n",
			"CONFIG_THIRD":  "y",
		})
	})
}

func TestResolveConfigChoiceMemberDependsOnHiddenDefBoolGate(t *testing.T) {
	resolved := mustResolveConfig(t, `
mainmenu "Test"

config GATE
	def_bool y

choice
	prompt "Backend"
	default FIRST

config FIRST
	bool "First"

config SECOND
	bool "Second"
	depends on GATE

endchoice
`, map[string]string{
		"CONFIG_SECOND": "y",
	})
	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_GATE":   "y",
		"CONFIG_FIRST":  "n",
		"CONFIG_SECOND": "y",
	})
}

func TestResolveConfigAllNoConfigUsesChoiceDefault(t *testing.T) {
	fixture := `
mainmenu "Test"

choice
	prompt "Mode"
	default MODE_B

config MODE_A
	bool "Mode A"

config MODE_B
	bool "Mode B"

endchoice
`
	resolved := mustResolveConfigWithOptions(t, fixture, nil, ResolveConfigOptions{
		AllNoConfig: true,
	})
	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_MODE_A": "n",
		"CONFIG_MODE_B": "y",
	})

	explicit := mustResolveConfigWithOptions(t, fixture, map[string]string{
		"CONFIG_MODE_A": "y",
	}, ResolveConfigOptions{
		AllNoConfig: true,
	})
	wantConfigValues(t, explicit, map[string]string{
		"CONFIG_MODE_A": "y",
		"CONFIG_MODE_B": "n",
	})
}

func TestResolveConfigAllNoConfigFallsBackToVisibleChoiceMember(t *testing.T) {
	fixture := `
mainmenu "Test"

config GATE
	bool
	default y

choice
	prompt "Mode"
	default MODE_A

config MODE_A
	bool "Mode A"
	depends on !GATE

config MODE_B
	bool "Mode B"
	depends on GATE

endchoice

config MODE_VALUE
	int
	default 1 if MODE_A
	default 2 if MODE_B
`
	resolved := mustResolveConfigWithOptions(t, fixture, nil, ResolveConfigOptions{
		AllNoConfig: true,
	})
	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_GATE":       "y",
		"CONFIG_MODE_A":     "n",
		"CONFIG_MODE_B":     "y",
		"CONFIG_MODE_VALUE": "2",
	})
}

func TestResolveConfigPromptlessChoiceUsesFirstVisibleMember(t *testing.T) {
	fixture := `
mainmenu "Test"

config HAVE_MODE_A
	bool
	default y

choice

config MODE_A
	bool "Mode A"
	depends on HAVE_MODE_A

config MODE_B
	bool "Mode B"

endchoice

config MODE_VALUE
	int
	default 1 if MODE_A
	default 2 if MODE_B
`
	resolved := mustResolveConfigWithOptions(t, fixture, nil, ResolveConfigOptions{
		AllNoConfig: true,
	})
	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_HAVE_MODE_A": "y",
		"CONFIG_MODE_A":      "y",
		"CONFIG_MODE_B":      "n",
		"CONFIG_MODE_VALUE":  "1",
	})
}

func TestResolveConfigScalarDefaultFromChoice(t *testing.T) {
	resolved := mustResolveConfig(t, `
mainmenu "Test"

choice
	prompt "Timer frequency"
	default HZ_250
	help
	  Choice-level help must not consume indented child config entries.

config HZ_100
	bool "100 HZ"

config HZ_250
	bool "250 HZ"

endchoice

config HZ
	int
	default 100 if HZ_100
	default 250 if HZ_250
`, nil)

	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_HZ":     "250",
		"CONFIG_HZ_100": "n",
		"CONFIG_HZ_250": "y",
	})
}

func TestResolveConfigChoiceDependsBlocksMembers(t *testing.T) {
	resolved := mustResolveConfig(t, `
mainmenu "Test"

config GATE
	bool "Gate"

choice
	prompt "Backend"
	depends on GATE
	default FIRST

config FIRST
	bool "First"

config SECOND
	bool "Second"

endchoice
`, map[string]string{
		"CONFIG_FIRST": "y",
	})

	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_FIRST":  "n",
		"CONFIG_SECOND": "n",
	})
}

func TestResolveConfigDefaultDependingOnDisabledChoice(t *testing.T) {
	resolved := mustResolveConfig(t, `
mainmenu "Test"

config ARCH_SPARSEMEM_ENABLE
	def_bool y

config ARCH_FLATMEM_ENABLE
	def_bool n

config ARCH_SPARSEMEM_DEFAULT
	def_bool y

config ARCH_SELECT_MEMORY_MODEL
	def_bool y
	depends on ARCH_SPARSEMEM_ENABLE && ARCH_FLATMEM_ENABLE

config SELECT_MEMORY_MODEL
	def_bool y
	depends on ARCH_SELECT_MEMORY_MODEL

choice
	prompt "Memory model"
	depends on SELECT_MEMORY_MODEL
	default SPARSEMEM_MANUAL if ARCH_SPARSEMEM_DEFAULT
	default FLATMEM_MANUAL

config FLATMEM_MANUAL
	bool "Flat Memory"
	depends on !ARCH_SPARSEMEM_ENABLE || ARCH_FLATMEM_ENABLE

config SPARSEMEM_MANUAL
	bool "Sparse Memory"
	depends on ARCH_SPARSEMEM_ENABLE

endchoice

config SPARSEMEM
	def_bool y
	depends on (!SELECT_MEMORY_MODEL && ARCH_SPARSEMEM_ENABLE) || SPARSEMEM_MANUAL

config FLATMEM
	def_bool y
	depends on !SPARSEMEM || FLATMEM_MANUAL
`, nil)

	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_SELECT_MEMORY_MODEL": "n",
		"CONFIG_SPARSEMEM":           "y",
		"CONFIG_FLATMEM":             "n",
	})
}

func TestResolveConfigNegativeDependencyDoesNotCreateSubmenu(t *testing.T) {
	resolved := mustResolveConfig(t, `
mainmenu "Test"

config A
	bool "A"

config B
	def_bool y
	depends on !A
`, nil)

	wantConfigValues(t, resolved, map[string]string{
		"CONFIG_A": "n",
		"CONFIG_B": "y",
	})
}

func mustResolveConfig(t *testing.T, fixture string, raw map[string]string) *ResolvedConfig {
	t.Helper()
	return mustResolveConfigWithOptions(t, fixture, raw, ResolveConfigOptions{})
}

func mustResolveConfigWithOptions(t *testing.T, fixture string, raw map[string]string, opts ResolveConfigOptions) *ResolvedConfig {
	t.Helper()
	tree, err := Parse(context.Background(), strings.NewReader(fixture), "Kconfig", Options{})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	resolved, err := tree.ResolveConfigWithOptions(raw, opts)
	if err != nil {
		t.Fatalf("ResolveConfig() failed: %v", err)
	}
	return resolved
}

func wantConfigValues(t *testing.T, resolved *ResolvedConfig, want map[string]string) {
	t.Helper()
	for key, wantValue := range want {
		if got := resolved.Value(key); got != wantValue {
			t.Fatalf("%s = %q, want %q; effective=%#v", key, got, wantValue, resolved.Effective)
		}
	}
}

func wantConfigWriteSet(t *testing.T, resolved *ResolvedConfig, want map[string]bool) {
	t.Helper()
	for key, wantValue := range want {
		if got := resolved.ShouldWrite(key); got != wantValue {
			t.Fatalf("ShouldWrite(%s) = %t, want %t; written=%#v effective=%#v", key, got, wantValue, resolved.Written, resolved.Effective)
		}
	}
}
