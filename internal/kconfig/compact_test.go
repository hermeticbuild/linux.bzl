package kconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	build "github.com/bazelbuild/buildtools/build"
)

const compactKconfigFixture = `
mainmenu "Compact"

config MODULES
	tristate "Modules"
	modules

config NET
	tristate "Networking"

config DEBUG
	bool "Debug"

config FORCE_NET
	bool "Force networking"
	select NET

config EFI_STUB
	bool "EFI stub"
`

const compactKbuildFixture = `
obj-y += init.o
obj-$(CONFIG_NET) += net/core.o
obj-$(CONFIG_DEBUG) += debug.o
ccflags-y += -Wall
ccflags-$(CONFIG_NET) += -DCONFIG_NET_SEEN
CFLAGS_net/core.o += -DNET_CORE
`

func TestCompactMetadataSharesUnrelatedObjectVariants(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{
		{
			Name: "base",
			Flags: map[string]string{
				"CONFIG_NET": "y",
			},
		},
		{
			Name: "debug",
			Flags: map[string]string{
				"CONFIG_DEBUG": "y",
				"CONFIG_NET":   "y",
			},
		},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	base := configByName(metadata, "base")
	debug := configByName(metadata, "debug")
	if base == nil || debug == nil {
		t.Fatalf("missing configs in metadata: %#v", metadata.Configs)
	}
	baseInit := objectTarget(metadata, base, "init.o")
	debugInit := objectTarget(metadata, debug, "init.o")
	if baseInit == "" || debugInit == "" {
		t.Fatalf("missing init.o target: base=%q debug=%q", baseInit, debugInit)
	}
	if baseInit != debugInit {
		t.Fatalf("unrelated CONFIG_DEBUG changed init.o target: base=%q debug=%q", baseInit, debugInit)
	}
	if objectTarget(metadata, base, "debug.o") != "" {
		t.Fatalf("base config unexpectedly includes debug.o")
	}
	if objectTarget(metadata, debug, "debug.o") == "" {
		t.Fatalf("debug config does not include debug.o")
	}
}

func TestCompactActionGroupsUseConcreteRecipeAndReachability(t *testing.T) {
	variant := func(target, contentID, flags string) CompactObjectVariant {
		return CompactObjectVariant{
			Target:    target,
			ContentID: strings.Repeat(contentID, 64),
			Object:    target + ".o",
			Source:    target + ".c",
			Mode:      "y",
			Flags:     []string{flags},
		}
	}
	metadata := &CompactMetadata{
		Configs: []CompactConfig{
			{Name: "base", ObjectTargets: []string{"shared_a", "shared_b"}},
			{Name: "lz4", ObjectTargets: []string{"shared_a", "shared_b"}},
			{Name: "debug", ObjectTargets: []string{"shared_a", "shared_b", "debug"}},
			{Name: "btf", ObjectTargets: []string{"btf"}},
		},
		ObjectVariants: []CompactObjectVariant{
			variant("shared_a", "1", "-DCOMMON"),
			variant("shared_b", "2", "-DCOMMON"),
			variant("debug", "3", "-DCOMMON"),
			variant("btf", "4", "-DBTF"),
			variant("unreachable", "5", "-DCOMMON"),
		},
	}
	groups, err := metadata.deriveActionGroups()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(groups), 3; got != want {
		t.Fatalf("action group count = %d, want %d: %#v", got, want, groups)
	}
	owners := map[string]CompactActionGroup{}
	for _, group := range groups {
		for _, target := range group.ObjectTargets {
			owners[target] = group
		}
	}
	if _, ok := owners["unreachable"]; ok {
		t.Fatal("unreachable object received an action group")
	}
	if owners["shared_a"].ID != owners["shared_b"].ID {
		t.Fatalf("same concrete recipe/reachability did not group: %#v", groups)
	}
	if got, want := owners["shared_a"].ReachableConfigs, []string{"base", "debug", "lz4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shared reachability = %v, want %v", got, want)
	}
	if owners["debug"].ID == owners["shared_a"].ID {
		t.Fatal("different reachability collapsed into one action group")
	}
	if owners["btf"].RecipeID == owners["shared_a"].RecipeID {
		t.Fatal("different concrete flags collapsed into one recipe")
	}
}

func TestCompactActionGroupRuleNameIgnoresMembership(t *testing.T) {
	first := CompactActionGroup{
		ID:               strings.Repeat("a", 64),
		RecipeID:         strings.Repeat("1", 64),
		ReachableConfigs: []string{"base", "lz4"},
		ObjectTargets:    []string{"first"},
	}
	second := CompactActionGroup{
		ID:               strings.Repeat("b", 64),
		RecipeID:         first.RecipeID,
		ReachableConfigs: append([]string(nil), first.ReachableConfigs...),
		ObjectTargets:    []string{"first", "second"},
	}
	if got, want := compactActionGroupRuleName(second), compactActionGroupRuleName(first); got != want {
		t.Fatalf("membership change renamed action group: got %q, want %q", got, want)
	}

	second.RecipeID = strings.Repeat("2", 64)
	if compactActionGroupRuleName(second) == compactActionGroupRuleName(first) {
		t.Fatal("different recipes produced the same action-group rule name")
	}
}

func TestCompactGroupedCompileFallbackCoversSpecialActionShapes(t *testing.T) {
	base := CompactObjectVariant{
		Target: "object",
		Object: "object.o",
		Source: "object.c",
		Mode:   "y",
		Flags:  []string{"-Wall"},
	}
	tests := map[string]func(*CompactObjectVariant){
		"generated header dependency": func(v *CompactObjectVariant) { v.Deps = []string{"generated"} },
		"remove flags":                func(v *CompactObjectVariant) { v.RemoveFlags = []string{"-pg"} },
		"generated object":            func(v *CompactObjectVariant) { v.Object = "arch/x86/kernel/cpu/capflags.o" },
		"certificate fail closed":     func(v *CompactObjectVariant) { v.Object = "certs/system_certificates.o" },
		"perlasm":                     func(v *CompactObjectVariant) { v.Object = "lib/crypto/arm64/sha256-core.o" },
		"asn1":                        func(v *CompactObjectVariant) { v.Object = "security/keys/foo.asn1.o" },
		"post compile objcopy":        func(v *CompactObjectVariant) { v.Object = "arch/arm64/kernel/foo.pi.o" },
		"shipped source":              func(v *CompactObjectVariant) { v.Source = "object.c_shipped" },
		"unsupported source":          func(v *CompactObjectVariant) { v.Source = "object.dts" },
		"object local directory":      func(v *CompactObjectVariant) { v.Flags = []string{"-I$(obj)"} },
		"temporary version header":    func(v *CompactObjectVariant) { v.Flags = []string{"-include", "utsversion-tmp.h"} },
		"module LTO root": func(v *CompactObjectVariant) {
			v.Mode = "m"
			v.ModuleRoot = true
			v.configFragment = map[string]string{"CONFIG_LTO_CLANG": "y"}
		},
		"forced module LTO objtool": func(v *CompactObjectVariant) {
			v.Mode = "m"
			v.ObjtoolForce = true
			v.configFragment = map[string]string{"CONFIG_LTO_CLANG": "y"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			variant := base
			mutate(&variant)
			if reason := (&CompactMetadata{}).groupedCompileFallbackReason(variant); reason == "" {
				t.Fatalf("special action shape was incorrectly accepted for direct grouping: %#v", variant)
			}
		})
	}
}

func TestCompactMetadataUsesEffectiveSelectedValues(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{
		{
			Name: "selected",
			Flags: map[string]string{
				"CONFIG_FORCE_NET": "y",
			},
		},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	config := configByName(metadata, "selected")
	target := objectTarget(metadata, config, "net/core.o")
	if target == "" {
		t.Fatalf("select FORCE_NET did not make CONFIG_NET object visible: %#v", metadata.Configs)
	}
	variant := variantByTarget(metadata, target)
	if got := variant.configFragment["CONFIG_NET"]; got != "y" {
		t.Fatalf("net/core.o fragment CONFIG_NET = %q, want y", got)
	}
}

func TestCompactMetadataSplitsImageAndModuleObjects(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "Modules"

config MODULES
	bool "Modules"
	modules

config NET
	tristate "Networking"
`)
	kb, err := ParseKbuild(strings.NewReader(`obj-y += init.o
obj-$(CONFIG_NET) += net.o
obj-m += trace.o helper.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{
		{Name: "module", Flags: map[string]string{"CONFIG_MODULES": "y", "CONFIG_NET": "m"}},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	config := configByName(metadata, "module")
	if objectTarget(metadata, config, "init.o") == "" {
		t.Fatalf("built-in object missing from image targets: %#v", config)
	}
	if target := objectTarget(metadata, config, "net.o"); target != "" {
		t.Fatalf("module object %q leaked into image targets: %#v", target, config)
	}
	moduleTarget := moduleObjectTarget(metadata, config, "net.o")
	if moduleTarget == "" {
		t.Fatalf("module object missing from module targets: %#v", config)
	}
	if variant := variantByTarget(metadata, moduleTarget); variant.Mode != "m" {
		t.Fatalf("module object mode = %q, want m", variant.Mode)
	}
	var moduleObjects []string
	for _, target := range config.ModuleObjectTargets {
		moduleObjects = append(moduleObjects, variantByTarget(metadata, target).Object)
	}
	if got, want := moduleObjects, []string{"net.o", "trace.o", "helper.o"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("module object order = %v, want %v", got, want)
	}
}

func TestCompactMetadataAppliesPerObjectKasanFlagsToBuiltinsAndModules(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "Sanitizers"

config MODULES
	bool "Modules"
	modules

config KASAN
	bool "KASAN"

config KASAN_GENERIC
	bool "Generic KASAN"

config KASAN_INLINE
	bool "Inline KASAN"

config KASAN_STACK
	bool "KASAN stack"

config KASAN_SHADOW_OFFSET
	hex "KASAN shadow offset"

config CC_HAS_KASAN_MEMINTRINSIC_PREFIX
	bool "KASAN memintrinsic prefix"
`)
	kb, err := ParseKbuild(strings.NewReader(`obj-y += builtin.o disabled.o
obj-m += module.o
KASAN_SANITIZE_disabled.o := n
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{
		Name: "kasan",
		Flags: map[string]string{
			"CONFIG_CC_HAS_KASAN_MEMINTRINSIC_PREFIX": "y",
			"CONFIG_KASAN":               "y",
			"CONFIG_KASAN_GENERIC":       "y",
			"CONFIG_KASAN_INLINE":        "y",
			"CONFIG_KASAN_SHADOW_OFFSET": "0xdffffc0000000000",
			"CONFIG_KASAN_STACK":         "y",
			"CONFIG_MODULES":             "y",
		},
	}}, CompactMetadataOptions{})
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}

	config := configByName(metadata, "kasan")
	for _, target := range []string{
		objectTarget(metadata, config, "builtin.o"),
		moduleObjectTarget(metadata, config, "module.o"),
	} {
		variant := variantByTarget(metadata, target)
		if !slices.Contains(variant.Flags, "-fsanitize=kernel-address") {
			t.Fatalf("%s mode=%s missing KASAN flags: %#v", variant.Object, variant.Mode, variant.Flags)
		}
	}
	disabled := variantByTarget(metadata, objectTarget(metadata, config, "disabled.o"))
	if slices.Contains(disabled.Flags, "-fsanitize=kernel-address") {
		t.Fatalf("disabled object received KASAN flags: %#v", disabled.Flags)
	}
}

func TestCompactMetadataExpandsUbsanTrapVariableForNvhe(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "nVHE UBSAN"

config UBSAN
	bool "UBSAN"

config UBSAN_BOOL
	bool "UBSAN bool"
`)
	dir := t.TempDir()
	kbuild := filepath.Join(dir, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`obj-y += switch.nvhe.o
UBSAN_SANITIZE := y
ccflags-y += $(CFLAGS_UBSAN_TRAP)
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{
			"CFLAGS_UBSAN_TRAP": "-fsanitize-trap=undefined",
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{
		Name: "ubsan",
		Flags: map[string]string{
			"CONFIG_UBSAN":      "y",
			"CONFIG_UBSAN_BOOL": "y",
		},
	}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}
	variant := variantByTarget(metadata, objectTarget(metadata, configByName(metadata, "ubsan"), "switch.nvhe.o"))
	if got, want := variant.Flags, []string{"-fsanitize-trap=undefined", "-fsanitize=bool"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("nVHE UBSAN flags = %#v, want %#v", got, want)
	}
}

func TestCompactMetadataAppliesCompositeAssignmentReplacement(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "Composite"

config MMU
	bool "MMU"
`)
	kb, err := ParseKbuild(strings.NewReader(`obj-y += proc.o
proc-y := nommu.o task_nommu.o
proc-$(CONFIG_MMU) := task_mmu.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{
		{Name: "mmu", Flags: map[string]string{"CONFIG_MMU": "y"}},
		{Name: "nommu", Flags: nil},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	mmu := configByName(metadata, "mmu")
	if objectTarget(metadata, mmu, "proc.o") != "" {
		t.Fatalf("MMU config kept built-in composite parent in image targets")
	}
	if objectTarget(metadata, mmu, "task_mmu.o") == "" {
		t.Fatalf("MMU config missing replacement member: %#v", mmu)
	}
	if target := objectTarget(metadata, mmu, "nommu.o"); target != "" {
		t.Fatalf("MMU config kept replaced member %q", target)
	}

	nommu := configByName(metadata, "nommu")
	if objectTarget(metadata, nommu, "proc.o") != "" {
		t.Fatalf("NOMMU config kept built-in composite parent in image targets")
	}
	if objectTarget(metadata, nommu, "nommu.o") == "" || objectTarget(metadata, nommu, "task_nommu.o") == "" {
		t.Fatalf("NOMMU config missing default members: %#v", nommu)
	}
	if target := objectTarget(metadata, nommu, "task_mmu.o"); target != "" {
		t.Fatalf("NOMMU config kept replacement member %q", target)
	}
}

func TestCompactMetadataSetsCompositeMemberModName(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader(`obj-y += mlx4_core.o
mlx4_core-y += alloc.o main.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{Name: "base"}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	base := configByName(metadata, "base")
	if objectTarget(metadata, base, "mlx4_core.o") != "" {
		t.Fatalf("built-in composite parent leaked into image targets")
	}
	member := variantByTarget(metadata, objectTarget(metadata, base, "main.o"))
	if member.ModName != "mlx4_core" {
		t.Fatalf("composite member ModName = %q, want mlx4_core", member.ModName)
	}
}

func TestCompactMetadataRespectsKbuildConfigConditionalBranches(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader(`ifeq ($(CONFIG_NET),y)
obj-y += net_on.o
else
obj-y += net_off.o
endif
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{
		{Name: "on", Flags: map[string]string{"CONFIG_NET": "y"}},
		{Name: "off", Flags: nil},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	on := configByName(metadata, "on")
	if objectTarget(metadata, on, "net_on.o") == "" || objectTarget(metadata, on, "net_off.o") != "" {
		t.Fatalf("on config did not select exactly net_on.o: %#v", on)
	}
	off := configByName(metadata, "off")
	if objectTarget(metadata, off, "net_off.o") == "" || objectTarget(metadata, off, "net_on.o") != "" {
		t.Fatalf("off config did not select exactly net_off.o: %#v", off)
	}
}

func TestCompactMetadataKeepsKnownConditionalCompositeAppends(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader(`obj-$(CONFIG_NET) += mlx5_core.o
mlx5_core-y := main.o
ifneq ($(CONFIG_NET),)
	mlx5_core-y += lib/vxlan.o
endif
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{
		{Name: "on", Flags: map[string]string{"CONFIG_NET": "y"}},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	on := configByName(metadata, "on")
	if objectTarget(metadata, on, "mlx5_core.o") != "" {
		t.Fatalf("built-in composite parent leaked into image targets")
	}
	if target := objectTarget(metadata, on, "lib/vxlan.o"); target == "" {
		t.Fatalf("known conditional append did not add composite member: %#v", on)
	}
}

func TestCompactMetadataRespectsRootObjectReplacement(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader(`obj-y += sme.o
targets += $(obj-y)
obj-y := $(patsubst %.o,%.pi.o,$(obj-y))
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{Name: "base"}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	base := configByName(metadata, "base")
	if target := objectTarget(metadata, base, "sme.o"); target != "" {
		t.Fatalf("replaced root object leaked into image: %q", target)
	}
	if target := objectTarget(metadata, base, "sme.pi.o"); target == "" {
		t.Fatalf("replacement root object missing")
	}
}

func TestCompactMetadataPreservesObjtoolSettings(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := parseKbuild(strings.NewReader(`obj-y += startup.o
pi-objs := $(patsubst %.o,$(obj)/%.o,$(obj-y))
$(pi-objs): objtool-enabled = 1
$(pi-objs): objtool-args = $(if $(delay-objtool),,$(objtool-args-y)) --noabs
targets += $(obj-y)
obj-y := $(patsubst %.o,%.pi.o,$(obj-y))
obj-y += normal.o head.o ignored.pi.o efi.stub.o
obj-m += module.o
OBJECT_FILES_NON_STANDARD := y
OBJECT_FILES_NON_STANDARD_normal.o := n
OBJECT_FILES_NON_STANDARD_ignored.o := y
OBJECT_FILES_NON_STANDARD_efi.o := n
`), "Kbuild", map[string]string{"obj": "."}, "")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	for _, source := range []string{"normal.c", "head.c", "ignored.c", "efi.c", "startup.c", "module.c"} {
		writeCompactSource(t, sourceRoot, source)
	}
	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
		SourceRoot: sourceRoot,
	})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}
	config := configByName(metadata, "base")
	normal := variantByTarget(metadata, objectTarget(metadata, config, "normal.o"))
	head := variantByTarget(metadata, objectTarget(metadata, config, "head.o"))
	ignored := variantByTarget(metadata, objectTarget(metadata, config, "ignored.pi.o"))
	efi := variantByTarget(metadata, objectTarget(metadata, config, "efi.stub.o"))
	startup := variantByTarget(metadata, objectTarget(metadata, config, "startup.pi.o"))
	module := variantByTarget(metadata, moduleObjectTarget(metadata, config, "module.o"))
	if normal.ObjtoolDisabled {
		t.Fatal("normal.o did not preserve its per-object override of the directory setting")
	}
	if !head.ObjtoolDisabled {
		t.Fatal("head.o did not preserve directory OBJECT_FILES_NON_STANDARD")
	}
	if !ignored.ObjtoolDisabled {
		t.Fatal("ignored.pi.o did not inherit OBJECT_FILES_NON_STANDARD_ignored.o")
	}
	if !efi.ObjtoolDisabled {
		t.Fatal("efi.stub.o unexpectedly enabled objtool for its rewritten underlying efi.o")
	}
	if startup.ObjtoolDisabled || !startup.ObjtoolForce || !reflect.DeepEqual(startup.ObjtoolArgs, []string{"--noabs"}) {
		t.Fatalf("startup.pi.o objtool settings = disabled:%t force:%t args:%q", startup.ObjtoolDisabled, startup.ObjtoolForce, startup.ObjtoolArgs)
	}

	objectBuild, err := metadata.BuildFile(CompactBuildFileOptions{
		BaseConfig:         "base",
		Arch:               "x86",
		SourceLabelPackage: "@linux//",
		SourceObjtool:      "//linux:objtool",
		SourceRootLabel:    "@linux//:Kconfig",
	})
	if err != nil {
		t.Fatalf("BuildFile() failed: %v", err)
	}
	parsed, err := build.ParseBuild("objects.BUILD.bazel", objectBuild)
	if err != nil {
		t.Fatalf("generated object BUILD did not parse: %v\n%s", err, objectBuild)
	}
	plan, err := metadata.actionGraphPlan()
	if err != nil {
		t.Fatal(err)
	}
	ruleFor := func(target string) *build.Rule {
		if plan.LegacyTargets[target] {
			return parsed.RuleNamed(target)
		}
		return parsed.RuleNamed(plan.GroupNameByTarget[target])
	}
	if got := ruleFor(head.Target).AttrString("objtool"); got != "" {
		t.Fatalf("head.o objtool = %q, want omitted", got)
	}
	if got := ruleFor(normal.Target).AttrString("objtool"); got != "//linux:objtool" {
		t.Fatalf("normal.o objtool = %q, want //linux:objtool", got)
	}
	if got := ruleFor(module.Target).AttrString("objtool"); got != "" {
		t.Fatalf("module.o objtool = %q, want module-root processing only", got)
	}
	if !module.ModuleRoot || ruleFor(module.Target).AttrLiteral("module_root") != "True" {
		t.Fatalf("module.o did not preserve its single-module root marker")
	}
	if got := ruleFor(startup.Target).AttrStrings("objtool_args"); !reflect.DeepEqual(got, []string{"--noabs"}) {
		t.Fatalf("startup.pi.o objtool_args = %q, want [--noabs]", got)
	}
	if !strings.Contains(string(objectBuild), "objtool_force = True") {
		t.Fatalf("startup.pi.o BUILD rule is missing objtool_force:\n%s", objectBuild)
	}
}

func TestCompactMetadataObjtoolFootprintSplitsOnlyEnabledX86Objects(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "Objtool footprint"

config OBJTOOL
	bool "Objtool"

config HAVE_JUMP_LABEL_HACK
	bool "Jump-label hack"

config LTO_CLANG
	bool "Clang LTO"
`)
	kb, err := ParseKbuild(strings.NewReader(`
obj-y += normal.o nonstandard.o
obj-m += nonstandard_module.o
OBJECT_FILES_NON_STANDARD_nonstandard.o := y
OBJECT_FILES_NON_STANDARD_nonstandard_module.o := y
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	generate := func(t *testing.T, srcarch string) *CompactMetadata {
		t.Helper()
		root := t.TempDir()
		writeCompactSource(t, root, "normal.c")
		writeCompactSource(t, root, "nonstandard.c")
		writeCompactSource(t, root, "nonstandard_module.c")
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{
			{Name: "off", Flags: map[string]string{"CONFIG_OBJTOOL": "y"}},
			{Name: "on", Flags: map[string]string{
				"CONFIG_HAVE_JUMP_LABEL_HACK": "y",
				"CONFIG_OBJTOOL":              "y",
			}},
			{Name: "lto", Flags: map[string]string{
				"CONFIG_LTO_CLANG": "y",
				"CONFIG_OBJTOOL":   "y",
			}},
		}, CompactMetadataOptions{
			CompileEnvironmentABI: "object-abi-v1",
			SourceRoot:            root,
			Srcarch:               srcarch,
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
		}
		return metadata
	}

	t.Run("x86", func(t *testing.T) {
		metadata := generate(t, "x86")
		offConfig := configByName(metadata, "off")
		onConfig := configByName(metadata, "on")
		offNormal := variantByTarget(metadata, objectTarget(metadata, offConfig, "normal.o"))
		onNormal := variantByTarget(metadata, objectTarget(metadata, onConfig, "normal.o"))
		if offNormal.ContentID == onNormal.ContentID {
			t.Fatalf("objtool-only config did not split object content ID %q", offNormal.ContentID)
		}
		if offNormal.CompileEnvironment == onNormal.CompileEnvironment {
			t.Fatalf("objtool-only config did not split compile environment %q", offNormal.CompileEnvironment)
		}
		if got := offNormal.configFragment["CONFIG_OBJTOOL"]; got != "y" {
			t.Fatalf("off normal CONFIG_OBJTOOL = %q, want y", got)
		}
		if got := offNormal.configFragment["CONFIG_HAVE_JUMP_LABEL_HACK"]; got != "n" {
			t.Fatalf("off normal CONFIG_HAVE_JUMP_LABEL_HACK = %q, want n", got)
		}
		if got := onNormal.configFragment["CONFIG_HAVE_JUMP_LABEL_HACK"]; got != "y" {
			t.Fatalf("on normal CONFIG_HAVE_JUMP_LABEL_HACK = %q, want y", got)
		}

		payloadContent := func(environmentID string) string {
			t.Helper()
			payloadID := ""
			for _, environment := range metadata.CompileEnvironments {
				if environment.ID == environmentID {
					payloadID = environment.ConfigPayload
					break
				}
			}
			for _, payload := range metadata.ConfigPayloads {
				if payload.ID == payloadID {
					return payload.Content
				}
			}
			t.Fatalf("compile environment %q has no payload", environmentID)
			return ""
		}
		if content := payloadContent(onNormal.CompileEnvironment); !strings.Contains(content, "CONFIG_HAVE_JUMP_LABEL_HACK=y\n") {
			t.Fatalf("on normal payload does not carry objtool-only config:\n%s", content)
		}

		offNonstandard := variantByTarget(metadata, objectTarget(metadata, offConfig, "nonstandard.o"))
		onNonstandard := variantByTarget(metadata, objectTarget(metadata, onConfig, "nonstandard.o"))
		if offNonstandard.ContentID != onNonstandard.ContentID ||
			offNonstandard.CompileEnvironment != onNonstandard.CompileEnvironment {
			t.Fatalf(
				"OBJECT_FILES_NON_STANDARD object split on objtool config:\noff=%#v\non=%#v",
				offNonstandard,
				onNonstandard,
			)
		}
		offModule := variantByTarget(metadata, moduleObjectTarget(metadata, offConfig, "nonstandard_module.o"))
		onModule := variantByTarget(metadata, moduleObjectTarget(metadata, onConfig, "nonstandard_module.o"))
		ltoModule := variantByTarget(
			metadata,
			moduleObjectTarget(metadata, configByName(metadata, "lto"), "nonstandard_module.o"),
		)
		if offModule.ContentID != onModule.ContentID ||
			offModule.CompileEnvironment != onModule.CompileEnvironment {
			t.Fatalf(
				"nonstandard module split on non-action objtool config:\noff=%#v\non=%#v",
				offModule,
				onModule,
			)
		}
		if offModule.ContentID == ltoModule.ContentID ||
			offModule.CompileEnvironment == ltoModule.CompileEnvironment {
			t.Fatalf(
				"nonstandard single module did not split on CONFIG_LTO_CLANG:\noff=%#v\nlto=%#v",
				offModule,
				ltoModule,
			)
		}
		if got := ltoModule.configFragment["CONFIG_LTO_CLANG"]; got != "y" {
			t.Fatalf("LTO nonstandard module CONFIG_LTO_CLANG = %q, want y", got)
		}
	})

	t.Run("arm64", func(t *testing.T) {
		metadata := generate(t, "arm64")
		offConfig := configByName(metadata, "off")
		onConfig := configByName(metadata, "on")
		ltoConfig := configByName(metadata, "lto")
		offNormal := variantByTarget(metadata, objectTarget(metadata, offConfig, "normal.o"))
		onNormal := variantByTarget(metadata, objectTarget(metadata, onConfig, "normal.o"))
		if offNormal.ContentID != onNormal.ContentID ||
			offNormal.CompileEnvironment != onNormal.CompileEnvironment {
			t.Fatalf("arm64 object split on x86 objtool config:\noff=%#v\non=%#v", offNormal, onNormal)
		}
		offModule := variantByTarget(metadata, moduleObjectTarget(metadata, offConfig, "nonstandard_module.o"))
		ltoModule := variantByTarget(metadata, moduleObjectTarget(metadata, ltoConfig, "nonstandard_module.o"))
		if offModule.ContentID == ltoModule.ContentID ||
			offModule.CompileEnvironment == ltoModule.CompileEnvironment {
			t.Fatalf(
				"arm64 nonstandard single module did not split on CONFIG_LTO_CLANG:\noff=%#v\nlto=%#v",
				offModule,
				ltoModule,
			)
		}
	})
}

func TestCompactMetadataPreservesModuleObjtoolShape(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader(`obj-m += single.o multi.o
multi-y := member.o skipped.o forced.o
OBJECT_FILES_NON_STANDARD_skipped.o := y
forced.o: objtool-enabled = 1
forced.o: objtool-args = --custom
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	for _, source := range []string{"single.c", "member.c", "skipped.c", "forced.c"} {
		writeCompactSource(t, sourceRoot, source)
	}
	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
		SourceRoot: sourceRoot,
	})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}
	config := configByName(metadata, "base")
	single := variantByTarget(metadata, moduleObjectTarget(metadata, config, "single.o"))
	multi := variantByTarget(metadata, moduleObjectTarget(metadata, config, "multi.o"))
	members := map[string]CompactObjectVariant{}
	for _, target := range multi.Members {
		variant := variantByTarget(metadata, target)
		members[variant.Object] = variant
	}
	member := members["member.o"]
	skipped := members["skipped.o"]
	forced := members["forced.o"]
	if !single.ModuleRoot || len(single.Members) != 0 || single.ObjtoolDisabled {
		t.Fatalf("single.o metadata = root:%t members:%q disabled:%t", single.ModuleRoot, single.Members, single.ObjtoolDisabled)
	}
	if !multi.ModuleRoot || len(multi.Members) != 3 {
		t.Fatalf("multi.o metadata = root:%t members:%q", multi.ModuleRoot, multi.Members)
	}
	if member.ModuleRoot || member.ObjtoolDisabled {
		t.Fatalf("member.o metadata = root:%t disabled:%t", member.ModuleRoot, member.ObjtoolDisabled)
	}
	if !skipped.ObjtoolDisabled {
		t.Fatal("skipped.o did not preserve OBJECT_FILES_NON_STANDARD")
	}
	if !forced.ObjtoolForce || !reflect.DeepEqual(forced.ObjtoolArgs, []string{"--custom"}) {
		t.Fatalf("forced.o objtool metadata = force:%t args:%q", forced.ObjtoolForce, forced.ObjtoolArgs)
	}

	objectBuild, err := metadata.BuildFile(CompactBuildFileOptions{
		BaseConfig:         "base",
		Arch:               "x86",
		SourceLabelPackage: "@linux//",
		SourceObjtool:      "//linux:objtool",
		SourceRootLabel:    "@linux//:Kconfig",
	})
	if err != nil {
		t.Fatalf("BuildFile() failed: %v", err)
	}
	parsed, err := build.ParseBuild("objects.BUILD.bazel", objectBuild)
	if err != nil {
		t.Fatalf("generated object BUILD did not parse: %v\n%s", err, objectBuild)
	}
	plan, err := metadata.actionGraphPlan()
	if err != nil {
		t.Fatal(err)
	}
	ruleFor := func(target string) *build.Rule {
		if plan.LegacyTargets[target] {
			return parsed.RuleNamed(target)
		}
		return parsed.RuleNamed(plan.GroupNameByTarget[target])
	}
	singleRule := ruleFor(single.Target)
	if singleRule.Kind() != "linux_object_action_group" ||
		singleRule.AttrLiteral("module_root") != "True" ||
		singleRule.AttrString("objtool") != "//linux:objtool" {
		t.Fatalf("single.o rule does not carry single-module objtool metadata:\n%s", objectBuild)
	}
	multiRule := ruleFor(multi.Target)
	if multiRule.Kind() != "linux_composite_object_action_group" || multiRule.AttrLiteral("module_root") != "True" {
		t.Fatalf("multi.o rule does not carry composite-module metadata:\n%s", objectBuild)
	}
	if got := ruleFor(member.Target).AttrString("objtool"); got != "//linux:objtool" {
		t.Fatalf("member.o objtool = %q, want //linux:objtool", got)
	}
	if got := ruleFor(skipped.Target).AttrString("objtool"); got != "" {
		t.Fatalf("skipped.o objtool = %q, want omitted", got)
	}
	forcedRule := ruleFor(forced.Target)
	if forcedRule.AttrLiteral("objtool_force") != "True" ||
		!reflect.DeepEqual(forcedRule.AttrStrings("objtool_args"), []string{"--custom"}) {
		t.Fatalf("forced.o rule does not preserve custom objtool settings:\n%s", objectBuild)
	}
}

func TestCompactMetadataPreservesKbuildRootOrder(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader(`obj-y += z.o
obj-y += a.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{Name: "base"}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	base := configByName(metadata, "base")
	if got, want := objectNames(metadata, base.ObjectTargets), []string{"z.o", "a.o"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ObjectTargets order = %#v, want %#v", got, want)
	}
}

func TestCompactMetadataPreservesKbuildDirectoryOrder(t *testing.T) {
	tree := mustParseCompactFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`obj-y += kernel/
obj-y += fs/
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	for subdir, content := range map[string]string{
		"fs":     "obj-y += configfs.o\n",
		"kernel": "obj-y += ksysfs.o\n",
	} {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) failed: %v", subdir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, subdir, "Makefile"), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%q/Makefile) failed: %v", subdir, err)
		}
	}

	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{RootDir: dir})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{Name: "base"}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	base := configByName(metadata, "base")
	if got, want := objectNames(metadata, base.ObjectTargets), []string{"kernel/ksysfs.o", "fs/configfs.o"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ObjectTargets order = %#v, want %#v", got, want)
	}
}

func TestCompactMetadataExpandsDirectoryAtAssignmentPosition(t *testing.T) {
	tree := mustParseCompactFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`obj-y := early.o child/ late.o
obj-y += head.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o755); err != nil {
		t.Fatalf("MkdirAll(child) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child", "Makefile"), []byte(`obj-y += child.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(child/Makefile) failed: %v", err)
	}

	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{RootDir: dir})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{Name: "base"}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	base := configByName(metadata, "base")
	if got, want := objectNames(metadata, base.ObjectTargets), []string{"early.o", "child/child.o", "late.o", "head.o"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ObjectTargets order = %#v, want %#v", got, want)
	}
}

func TestCompactMetadataAppendsSortedLibraryRoots(t *testing.T) {
	tree := mustParseCompactFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`obj-y += arch/lib/
obj-y += lib/
obj-y += drivers/
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	for subdir, content := range map[string]string{
		"arch/lib": "lib-y := z.o a.o\n",
		"drivers":  "obj-y += driver.o\n",
		"lib":      "lib-y := c.o b.o\nobj-y += builtin.o\n",
	} {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) failed: %v", subdir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, subdir, "Makefile"), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%q/Makefile) failed: %v", subdir, err)
		}
	}

	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{RootDir: dir})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
		LibraryDirs: []string{"arch/lib", "lib"},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	base := configByName(metadata, "base")
	want := []string{
		"lib/builtin.o",
		"drivers/driver.o",
		"arch/lib/a.o",
		"arch/lib/z.o",
		"lib/b.o",
		"lib/c.o",
	}
	if got := objectNames(metadata, base.ObjectTargets); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ObjectTargets order = %#v, want %#v", got, want)
	}
}

func TestCompactMetadataScopesRootObjectReplacementToDirectory(t *testing.T) {
	tree := mustParseCompactFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`obj-y += first/ second/
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "first"), 0o755); err != nil {
		t.Fatalf("MkdirAll(first) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "first", "Makefile"), []byte(`obj-y += old.o
obj-y := new.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(first/Makefile) failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "second"), 0o755); err != nil {
		t.Fatalf("MkdirAll(second) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second", "Makefile"), []byte(`obj-y += keep.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(second/Makefile) failed: %v", err)
	}

	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{RootDir: dir})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{Name: "base"}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	base := configByName(metadata, "base")
	if target := objectTarget(metadata, base, "first/old.o"); target != "" {
		t.Fatalf("replaced object leaked into first directory: %q", target)
	}
	if target := objectTarget(metadata, base, "first/new.o"); target == "" {
		t.Fatalf("replacement object missing from first directory")
	}
	if target := objectTarget(metadata, base, "second/keep.o"); target == "" {
		t.Fatalf("second directory object was incorrectly replaced")
	}
}

func TestCompactMetadataDoesNotLinkSubdirDescents(t *testing.T) {
	tree := mustParseCompactFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`obj-y += linked/
subdir-y += side
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	for _, subdir := range []string{"linked", "side"} {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) failed: %v", subdir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, subdir, "Makefile"), []byte(`obj-y += main.o
`), 0o644); err != nil {
			t.Fatalf("WriteFile(%q/Makefile) failed: %v", subdir, err)
		}
	}
	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{RootDir: dir})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{Name: "base", Flags: nil}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}
	config := configByName(metadata, "base")
	if objectTarget(metadata, config, "linked/main.o") == "" {
		t.Fatalf("linked directory object missing from image roots: %#v", config)
	}
	if target := objectTarget(metadata, config, "side/main.o"); target != "" {
		t.Fatalf("subdir-y object leaked into image roots as %q", target)
	}
}

func TestCompactMetadataLinksArchiveDirectoryRoots(t *testing.T) {
	tree := mustParseCompactFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`libs-y += $(objtree)/libstub/lib.a
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "libstub"), 0o755); err != nil {
		t.Fatalf("MkdirAll(libstub) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "libstub", "Makefile"), []byte(`lib-y += entry.o helper.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(libstub/Makefile) failed: %v", err)
	}

	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{RootDir: dir})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{Name: "base", Flags: nil}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}
	config := configByName(metadata, "base")
	for _, object := range []string{"libstub/entry.o", "libstub/helper.o"} {
		if objectTarget(metadata, config, object) == "" {
			t.Fatalf("archive directory object %q missing from image roots: %#v", object, config)
		}
	}
}

func TestCompactMetadataLeavesRustObjectsToDedicatedSDK(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "Rust SDK"

config RUST
	bool "Rust"
`)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`obj-y += init.o
obj-$(CONFIG_RUST) += rust/
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "rust"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rust", "Makefile"), []byte(`obj-$(CONFIG_RUST) += core.o helpers/helpers.o kernel.o exports.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(rust/Makefile) failed: %v", err)
	}

	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{RootDir: dir})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{
		Name:  "base",
		Flags: map[string]string{"CONFIG_RUST": "y"},
	}}, CompactMetadataOptions{})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}
	config := configByName(metadata, "base")
	if objectTarget(metadata, config, "init.o") == "" {
		t.Fatal("non-Rust image object was removed")
	}
	for _, variant := range metadata.ObjectVariants {
		if strings.HasPrefix(variant.Object, "rust/") {
			t.Fatalf("dedicated Rust SDK object leaked into compact graph: %#v", variant)
		}
	}
}

func TestCompactMetadataKeepsNestedArchiveDirectoriesRootRelative(t *testing.T) {
	tree := mustParseCompactFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`obj-y += arch/arm64/
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "arch", "arm64"), 0o755); err != nil {
		t.Fatalf("MkdirAll(arch/arm64) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "arch", "arm64", "Makefile"), []byte(`libs-$(CONFIG_EFI_STUB) += $(objtree)/drivers/firmware/efi/libstub/lib.a
`), 0o644); err != nil {
		t.Fatalf("WriteFile(arch/arm64/Makefile) failed: %v", err)
	}
	libstub := filepath.Join(dir, "drivers", "firmware", "efi", "libstub")
	if err := os.MkdirAll(libstub, 0o755); err != nil {
		t.Fatalf("MkdirAll(libstub) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(libstub, "Makefile"), []byte(`lib-y += efi-stub-entry.stub.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(libstub/Makefile) failed: %v", err)
	}

	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{
		RootDir: dir,
		Variables: map[string]string{
			"CONFIG_EFI_STUB": "y",
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{Name: "base", Flags: map[string]string{"CONFIG_EFI_STUB": "y"}}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}
	config := configByName(metadata, "base")
	if objectTarget(metadata, config, "drivers/firmware/efi/libstub/efi-stub-entry.stub.o") == "" {
		t.Fatalf("root-relative archive directory object missing from image roots: %#v", config)
	}
	if target := objectTarget(metadata, config, "arch/arm64/drivers/firmware/efi/libstub/efi-stub-entry.stub.o"); target != "" {
		t.Fatalf("root-relative archive directory was prefixed by parent as %q", target)
	}
}

func TestCompactMetadataUsesRootMakefileDirectories(t *testing.T) {
	tree := mustParseCompactFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`obj-y += arch/arm64/
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "arch", "arm64"), 0o755); err != nil {
		t.Fatalf("MkdirAll(arch/arm64) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "arch", "arm64", "Kbuild"), []byte(`obj-y += kernel/
`), 0o644); err != nil {
		t.Fatalf("WriteFile(arch/arm64/Kbuild) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "arch", "arm64", "Makefile"), []byte(`libs-$(CONFIG_EFI_STUB) += $(objtree)/drivers/firmware/efi/libstub/lib.a
`), 0o644); err != nil {
		t.Fatalf("WriteFile(arch/arm64/Makefile) failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "arch", "arm64", "kernel"), 0o755); err != nil {
		t.Fatalf("MkdirAll(arch/arm64/kernel) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "arch", "arm64", "kernel", "Makefile"), []byte(`obj-y += setup.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(arch/arm64/kernel/Makefile) failed: %v", err)
	}
	libstub := filepath.Join(dir, "drivers", "firmware", "efi", "libstub")
	if err := os.MkdirAll(libstub, 0o755); err != nil {
		t.Fatalf("MkdirAll(libstub) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(libstub, "Makefile"), []byte(`lib-y += efi-stub-entry.stub.o
`), 0o644); err != nil {
		t.Fatalf("WriteFile(libstub/Makefile) failed: %v", err)
	}

	kb, err := ParseKbuildDirectoryTree(filepath.Join(dir, "Kbuild"), KbuildOptions{
		RootDir:       dir,
		RootMakefiles: []string{filepath.Join(dir, "arch", "arm64", "Makefile")},
		Variables: map[string]string{
			"CONFIG_EFI_STUB": "y",
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{Name: "base", Flags: map[string]string{"CONFIG_EFI_STUB": "y"}}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}
	config := configByName(metadata, "base")
	for _, object := range []string{
		"arch/arm64/kernel/setup.o",
		"drivers/firmware/efi/libstub/efi-stub-entry.stub.o",
	} {
		if objectTarget(metadata, config, object) == "" {
			t.Fatalf("root makefile object %q missing from image roots: %#v", object, config)
		}
	}
}

func TestCompactBuildFilesParse(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	sourceRoot := t.TempDir()
	writeCompactSource(t, sourceRoot, "init.c")
	writeCompactSource(t, sourceRoot, "net/core.c")
	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{
		{Name: "base", Flags: map[string]string{"CONFIG_NET": "y"}},
	}, CompactMetadataOptions{SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}

	objectBuild, err := metadata.BuildFile(CompactBuildFileOptions{
		BaseConfig:         "base",
		RuleLoadLabel:      "//rules:linux_objects.bzl",
		SourceLabelPackage: "//linux",
		SourceRootLabel:    "//linux:Kconfig",
	})
	if err != nil {
		t.Fatalf("BuildFile() failed: %v", err)
	}
	if _, err := build.ParseBuild("objects.BUILD.bazel", objectBuild); err != nil {
		t.Fatalf("object BUILD did not parse: %v\n%s", err, objectBuild)
	}

	if !strings.Contains(string(objectBuild), `load("//rules:linux_objects.bzl"`) ||
		!strings.Contains(string(objectBuild), `load("//rules:linux_object_groups.bzl"`) ||
		!strings.Contains(string(objectBuild), `"linux_object_action_group"`) ||
		!strings.Contains(string(objectBuild), `"linux_grouped_compact_image"`) {
		t.Fatalf("object BUILD does not use custom compact rule load label:\n%s", objectBuild)
	}
	for _, unused := range []string{
		`"linux_arm64_nvhe_object"`,
		`"linux_composite_object"`,
		`"linux_composite_object_action_group"`,
		`"linux_object"`,
		`"linux_object_action_group_import"`,
	} {
		if strings.Contains(string(objectBuild), unused) {
			t.Fatalf("object BUILD loads unused rule %s:\n%s", unused, objectBuild)
		}
	}

	imageBuild, err := metadata.imageBuildFile(CompactBuildFileOptions{
		BaseConfig:         "base",
		ObjectLabelPackage: "linux/objects",
		RuleLoadLabel:      "//rules:linux_objects.bzl",
	})
	if err != nil {
		t.Fatalf("imageBuildFile() failed: %v", err)
	}
	if _, err := build.ParseBuild("images.BUILD.bazel", imageBuild); err != nil {
		t.Fatalf("image BUILD did not parse: %v\n%s", err, imageBuild)
	}
	if !strings.Contains(string(imageBuild), `"//linux/objects:`) {
		t.Fatalf("image BUILD does not reference object package:\n%s", imageBuild)
	}
	if !strings.Contains(string(imageBuild), `load("//rules:linux_object_groups.bzl"`) ||
		!strings.Contains(string(imageBuild), `"linux_grouped_compact_image"`) {
		t.Fatalf("image BUILD does not use custom compact rule load label:\n%s", imageBuild)
	}
	if strings.Contains(string(imageBuild), "require_real") {
		t.Fatalf("image BUILD contains removed require_real compatibility attribute:\n%s", imageBuild)
	}
}

func TestCompactContentGraphObjectBuildUsesOneExactCompileEnvironmentIndex(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "init.c", "#include \"shared.h\"\n")
	mustWriteSource(t, sourceRoot, "shared.h", "#define SHARED 1\n")
	writeCompactSource(t, sourceRoot, "net/core.c")
	writeCompactSource(t, sourceRoot, "debug.c")
	writeCompactContentGraphForcedInputs(t, sourceRoot)

	common := CompactMetadataOptions{
		SourceRoot:            sourceRoot,
		Srcarch:               "x86",
		CompileEnvironmentABI: "clang-21-linux-object-abi-v1",
	}
	metadata, err := tree.CompactMetadataBatchWithOptions([]NamedConfig{
		{
			Name:  "base",
			Flags: map[string]string{"CONFIG_NET": "y"},
		},
		{
			Name: "debug",
			Flags: map[string]string{
				"CONFIG_DEBUG": "y",
				"CONFIG_NET":   "y",
			},
		},
	}, common, func(config *ResolvedConfig) (CompactConfigGraph, error) {
		labels := map[string]string{
			"base":  "//headers:z_base",
			"debug": "//headers:a_debug",
		}
		return CompactConfigGraph{
			Kbuild:                kb,
			GeneratedHeadersLabel: labels[config.Name],
		}, nil
	})
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}

	if got := len(metadata.GeneratedHeaderFamilies); got != 12 {
		t.Fatalf("generated header family count = %d, want 12: %#v", got, metadata.GeneratedHeaderFamilies)
	}
	for _, family := range metadata.GeneratedHeaderFamilies {
		if want := []string{"//headers:a_debug", "//headers:z_base"}; !reflect.DeepEqual(family.Labels, want) {
			t.Fatalf(
				"shared generated header family %s labels = %v, want %v",
				family.Name,
				family.Labels,
				want,
			)
		}
	}
	baseInit := objectTarget(metadata, configByName(metadata, "base"), "init.o")
	debugInit := objectTarget(metadata, configByName(metadata, "debug"), "init.o")
	if baseInit == "" || baseInit != debugInit {
		t.Fatalf("base/debug init targets = %q/%q, want one shared target", baseInit, debugInit)
	}
	initVariant := variantByTarget(metadata, baseInit)
	if len(initVariant.ContentID) != 64 || len(initVariant.CompileEnvironment) != 64 {
		t.Fatalf("init IDs = content %q environment %q, want full SHA-256 IDs", initVariant.ContentID, initVariant.CompileEnvironment)
	}
	var inputPaths []string
	initInputs, err := metadata.expandedSourceInputGroup(
		initVariant.SourceInputGroup,
		"init",
	)
	if err != nil {
		t.Fatalf("expand init source inputs: %v", err)
	}
	for _, input := range initInputs {
		inputPaths = append(inputPaths, input.Path)
		if len(input.Digest) != 64 {
			t.Fatalf("source input %q digest = %q, want full SHA-256", input.Path, input.Digest)
		}
	}
	wantInputs := []string{
		"include/linux/compiler-version.h",
		"include/linux/compiler_types.h",
		"include/linux/kconfig.h",
		"init.c",
		"shared.h",
	}
	if !reflect.DeepEqual(inputPaths, wantInputs) {
		t.Fatalf("init source inputs = %v, want %v", inputPaths, wantInputs)
	}
	metadataJSON, err := metadata.JSON()
	if err != nil {
		t.Fatalf("JSON() failed: %v", err)
	}
	serialized := string(metadataJSON)
	for _, redundant := range []string{`"source_inputs":`, `"config_fragment":`, `"fragment":`, `"image_target":`} {
		if strings.Contains(serialized, redundant) {
			t.Fatalf("content graph JSON retains redundant field %s:\n%s", redundant, metadataJSON)
		}
	}
	for _, required := range []string{`"source_files":`, `"source_input_groups":`, `"source_input_group":`} {
		if !strings.Contains(serialized, required) {
			t.Fatalf("content graph JSON omits indexed field %s:\n%s", required, metadataJSON)
		}
	}

	objectBuild, err := metadata.BuildFile(CompactBuildFileOptions{
		BaseConfig:         "base",
		Arch:               "x86",
		Version:            "6.18.39",
		SourceLabelPackage: "@linux//",
		SourceRootLabel:    "@linux//:Kconfig",
		Srcarch:            "x86",
	})
	if err != nil {
		t.Fatalf("BuildFile() failed: %v", err)
	}
	parsed, err := build.ParseBuild("objects.BUILD.bazel", objectBuild)
	if err != nil {
		t.Fatalf("generated object BUILD did not parse: %v\n%s", err, objectBuild)
	}
	index := parsed.RuleNamed("_compile_environment_index")
	if index == nil || index.Kind() != "linux_compile_environment_index" {
		t.Fatalf("generated object BUILD has no compile environment index:\n%s", objectBuild)
	}
	if got := index.AttrString("arch"); got != "x86" {
		t.Fatalf("compile environment index arch = %q, want x86", got)
	}
	if got := index.AttrString("version"); got != "6.18.39" {
		t.Fatalf("compile environment index version = %q, want 6.18.39", got)
	}
	if got := index.AttrString("expected_abi"); got != common.CompileEnvironmentABI {
		t.Fatalf("compile environment index expected_abi = %q, want %q", got, common.CompileEnvironmentABI)
	}
	if got, want := index.AttrStrings("generated_headers"), []string{"//headers:a_debug"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("compile environment generated_headers = %v, want label list %v", got, want)
	}
	referencedPayloads := map[string]bool{}
	for _, environment := range metadata.CompileEnvironments {
		referencedPayloads[environment.ConfigPayload] = true
	}
	payloads, ok := index.Attr("config_payloads").(*build.DictExpr)
	if !ok {
		t.Fatalf("indexed config payloads = %T, want dictionary", index.Attr("config_payloads"))
	}
	if len(payloads.List) != len(referencedPayloads) {
		t.Fatalf("indexed config payload count = %d, want %d referenced payloads", len(payloads.List), len(referencedPayloads))
	}
	sourceTree := parsed.RuleNamed("_source_tree")
	if sourceTree == nil {
		t.Fatalf("generated object BUILD has no source tree rule:\n%s", objectBuild)
	}
	for _, attr := range []string{
		"all_files",
		"arch_headers",
		"dtb_sources",
		"global_headers",
		"headers",
		"kbuild_files",
		"local_include_files",
		"lookup_files",
		"scripts_headers",
		"uapi_headers",
	} {
		if sourceTree.Attr(attr) != nil {
			t.Errorf("content graph source tree unexpectedly emits broad %s", attr)
		}
	}
	text := string(objectBuild)
	if strings.Count(text, "linux_compile_environment_index(") != 1 {
		t.Fatalf("compile environment index rule count != 1:\n%s", objectBuild)
	}
	sourceIndex := parsed.RuleNamed("_source_input_index")
	if sourceIndex == nil || sourceIndex.Kind() != "linux_source_input_index" {
		t.Fatalf("generated object BUILD has no source input index:\n%s", objectBuild)
	}
	if got := len(sourceIndex.AttrStrings("srcs")); got != len(metadata.SourceFiles) {
		t.Fatalf("indexed source count = %d, want %d", got, len(metadata.SourceFiles))
	}
	if sourceIndex.Attr("source_paths") != nil {
		t.Fatalf("source input index retains order-sensitive source_paths:\n%s", objectBuild)
	}
	if got := sourceIndex.AttrString("source_tree_info"); got != ":_source_tree" {
		t.Fatalf("source input index source_tree_info = %q, want :_source_tree", got)
	}
	for _, input := range metadata.SourceFiles {
		label := labelFor("@linux//", input.Path)
		if count := strings.Count(text, fmt.Sprintf("%q", label)); count != 1 {
			t.Fatalf("source label %q occurs %d times, want once", label, count)
		}
	}
	if strings.Contains(text, "linux_config(") {
		t.Fatalf("content graph object BUILD emitted per-object linux_config rules:\n%s", objectBuild)
	}
	if !strings.Contains(text, `"//headers:a_debug"`) || strings.Contains(text, `"//headers:z_base"`) {
		t.Fatalf("generated headers did not select the canonical shared label:\n%s", objectBuild)
	}
	plan, err := metadata.actionGraphPlan()
	if err != nil {
		t.Fatal(err)
	}
	initRule := parsed.RuleNamed(plan.GroupNameByTarget[baseInit])
	if initRule == nil {
		t.Fatalf("generated object BUILD has no init action group for %q:\n%s", baseInit, objectBuild)
	}
	if initRule.Kind() != "linux_object_action_group" {
		t.Fatalf("init action owner kind = %q, want linux_object_action_group", initRule.Kind())
	}
	for _, attr := range []string{"src", "source_includes", "source_includes_complete", "config_fragment"} {
		if initRule.Attr(attr) != nil {
			t.Fatalf("indexed init rule retains redundant %s:\n%s", attr, objectBuild)
		}
	}
	if got := initRule.AttrString("source_input_index"); got != ":_source_input_index" {
		t.Fatalf("init source_input_index = %q", got)
	}
	sourceFile, err := metadata.sourceFileIndex(initVariant.Source)
	if err != nil {
		t.Fatal(err)
	}
	if got := initRule.AttrString("compile_environment_index"); got != ":_compile_environment_index" {
		t.Fatalf("init compile_environment_index = %q", got)
	}
	for _, want := range []string{
		initVariant.CompileEnvironment,
		initVariant.ContentID,
		fmt.Sprintf(`\"source_input_file\":%d`, sourceFile),
		fmt.Sprintf(`\"source_input_group\":%d`, initVariant.SourceInputGroup),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("grouped init object spec omits %q:\n%s", want, objectBuild)
		}
	}
}

func TestCompactContentGraphGeneratedHeaderFamiliesDeduplicateAcrossConfigs(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "Generated header families"

config LOCALVERSION
	string "Local version"
	default ""
`)
	kb, err := ParseKbuild(strings.NewReader("obj-y += init.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "init.c", "#include <asm/unistd.h>\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	mustWriteSource(
		t,
		sourceRoot,
		"include/asm-generic/unistd.h",
		"#define GENERIC_UNISTD 1\n",
	)
	mustWriteSource(
		t,
		sourceRoot,
		"include/linux/compiler-version.h",
		`#ifdef GCC_PLUGINS
#include <generated/gcc-plugins.h>
#endif
#ifdef RANDSTRUCT
#include <generated/randstruct_hash.h>
#endif
#ifdef INTEGER_WRAP
#include <generated/integer-wrap.h>
#endif
`,
	)
	mustWriteSource(
		t,
		sourceRoot,
		"include/linux/kconfig.h",
		"#include <generated/autoconf.h>\n",
	)

	configs := []NamedConfig{
		{Name: "base", Flags: map[string]string{"CONFIG_LOCALVERSION": `"-base"`}},
		{Name: "debug", Flags: map[string]string{"CONFIG_LOCALVERSION": `"-debug"`}},
	}
	labels := map[string]string{
		"base":  "//headers:base",
		"debug": "//headers:debug",
	}
	metadata, err := tree.CompactMetadataBatchWithOptions(
		configs,
		CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "x86",
			CompileEnvironmentABI: "object-abi-v1",
			KernelVersion:         "6.18.0",
		},
		func(config *ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{
				Kbuild:                kb,
				GeneratedHeadersLabel: labels[config.Name],
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}

	families := func(name string) []CompactGeneratedHeaderFamily {
		t.Helper()
		var out []CompactGeneratedHeaderFamily
		for _, family := range metadata.GeneratedHeaderFamilies {
			if family.Name == name {
				out = append(out, family)
			}
		}
		return out
	}
	staticFamilies := families(compactGeneratedHeaderFamilyStatic)
	if len(staticFamilies) != 1 {
		t.Fatalf("static families = %#v, want one shared family", staticFamilies)
	}
	if want := []string{"//headers:base", "//headers:debug"}; !reflect.DeepEqual(
		staticFamilies[0].Labels,
		want,
	) {
		t.Fatalf("shared static labels = %v, want %v", staticFamilies[0].Labels, want)
	}
	if got := len(families(compactGeneratedHeaderFamilyUTSRelease)); got != 2 {
		t.Fatalf("utsrelease family count = %d, want 2", got)
	}
	if got := len(families(compactGeneratedHeaderFamilyASMOffsets)); got != 1 {
		t.Fatalf("asm_offsets family count = %d, want 1", got)
	}

	baseTarget := objectTarget(metadata, configByName(metadata, "base"), "init.o")
	debugTarget := objectTarget(metadata, configByName(metadata, "debug"), "init.o")
	if baseTarget == "" || baseTarget != debugTarget {
		t.Fatalf("base/debug init targets = %q/%q, want one shared target", baseTarget, debugTarget)
	}
	variant := variantByTarget(metadata, baseTarget)
	var environment CompactCompileEnvironment
	for _, candidate := range metadata.CompileEnvironments {
		if candidate.ID == variant.CompileEnvironment {
			environment = candidate
			break
		}
	}
	if want := []string{staticFamilies[0].ID}; !reflect.DeepEqual(
		environment.GeneratedHeaderFamilies,
		want,
	) {
		t.Fatalf(
			"init generated header families = %v, want shared static %v",
			environment.GeneratedHeaderFamilies,
			want,
		)
	}
}

func TestGeneratedHeaderFamilySelectionFailsClosed(t *testing.T) {
	families := compactGeneratedHeaderFamilySet{
		compactGeneratedHeaderFamilyAll: {
			ID:   "all-id",
			Name: compactGeneratedHeaderFamilyAll,
		},
		compactGeneratedHeaderFamilyStatic: {
			ID:   "static-id",
			Name: compactGeneratedHeaderFamilyStatic,
		},
	}
	if got, err := families.selectForAction(nil, true); err != nil ||
		!reflect.DeepEqual(got, []string{"all-id"}) {
		t.Fatalf("force-all selection = %v, %v, want [all-id], nil", got, err)
	}
	if got, err := families.selectForAction(
		[]string{"generated/autoconf.h", "asm/unistd.h"},
		false,
	); err != nil || !reflect.DeepEqual(got, []string{"static-id"}) {
		t.Fatalf("precise selection = %v, %v, want [static-id], nil", got, err)
	}
	if _, err := families.selectForAction(
		[]string{"generated/not-an-output.h"},
		false,
	); err == nil || !strings.Contains(err.Error(), "is unclassified") {
		t.Fatalf("unclassified selection error = %v, want fail-closed error", err)
	}
	if _, err := families.selectForAction(
		[]string{"generated/timeconst.h"},
		false,
	); err == nil || !strings.Contains(err.Error(), `unavailable family "timeconst"`) {
		t.Fatalf("missing precise-family error = %v, want unavailable family", err)
	}

	arm64Families := compactGeneratedHeaderFamilySet{
		compactGeneratedHeaderFamilyAll: {
			ID:   "arm64-all-id",
			Name: compactGeneratedHeaderFamilyAll,
		},
	}
	if got, err := arm64Families.selectForAction(
		[]string{"generated/autoconf.h"},
		false,
	); err != nil || len(got) != 0 {
		t.Fatalf("arm64 config-owned selection = %v, %v, want empty, nil", got, err)
	}
	for _, include := range []string{
		"generated/timeconst.h",
		"generated/arm64-provider-output.h",
		"asm/unistd.h",
	} {
		got, err := arm64Families.selectForAction([]string{include}, false)
		if err != nil || !reflect.DeepEqual(got, []string{"arm64-all-id"}) {
			t.Errorf("arm64 selection for %q = %v, %v, want [arm64-all-id], nil", include, got, err)
		}
	}
}

func TestCompactContentGraphArm64GeneratedIncludeSelectsMonolithicFamily(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"arm64 generated-header selection\"\n")
	kb, err := ParseKbuild(strings.NewReader("obj-y += init.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	for _, dir := range []string{
		"arch/arm64/kernel/vdso",
		"arch/arm64/kernel/vdso32",
	} {
		if err := os.MkdirAll(filepath.Join(sourceRoot, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) failed: %v", dir, err)
		}
	}
	mustWriteSource(t, sourceRoot, "init.c", "#include <generated/timeconst.h>\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	metadata, err := tree.CompactMetadataBatchWithOptions(
		[]NamedConfig{{Name: "arm64"}},
		CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "arm64",
			CompileEnvironmentABI: "arm64-object-abi-v1",
		},
		func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{
				Kbuild:                kb,
				GeneratedHeadersLabel: "//headers:arm64",
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}
	if len(metadata.GeneratedHeaderFamilies) != 1 ||
		metadata.GeneratedHeaderFamilies[0].Name != compactGeneratedHeaderFamilyAll {
		t.Fatalf(
			"arm64 generated-header families = %#v, want one all family",
			metadata.GeneratedHeaderFamilies,
		)
	}
	config := configByName(metadata, "arm64")
	variant := variantByTarget(metadata, objectTarget(metadata, config, "init.o"))
	var environment CompactCompileEnvironment
	for _, candidate := range metadata.CompileEnvironments {
		if candidate.ID == variant.CompileEnvironment {
			environment = candidate
			break
		}
	}
	want := []string{metadata.GeneratedHeaderFamilies[0].ID}
	if !reflect.DeepEqual(environment.GeneratedHeaderFamilies, want) {
		t.Fatalf(
			"arm64 init generated-header families = %v, want monolithic %v",
			environment.GeneratedHeaderFamilies,
			want,
		)
	}
}

func TestCompactContentGraphARMGeneratedSyscallIncludeSelectsMonolithicFamily(t *testing.T) {
	tree := mustParseString(t, `
config AEABI
	bool
`)
	kb, err := ParseKbuild(strings.NewReader("obj-y += arch/arm/kernel/entry-common.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "arch/arm/kernel/entry-common.S", `
#ifdef CONFIG_AEABI
#include <calls-eabi.S>
#else
#include <calls-oabi.S>
#endif
`)
	mustWriteSource(t, sourceRoot, "arch/arm/tools/syscall.tbl", "0 common restart_syscall sys_restart_syscall\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	metadata, err := tree.CompactMetadataBatchWithOptions(
		[]NamedConfig{{Name: "arm", Flags: map[string]string{"CONFIG_AEABI": "y"}}},
		CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "arm",
			CompileEnvironmentABI: "arm-object-abi-v1",
		},
		func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{
				Kbuild:                kb,
				GeneratedHeadersLabel: "//headers:arm",
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}
	if len(metadata.GeneratedHeaderFamilies) != 1 ||
		metadata.GeneratedHeaderFamilies[0].Name != compactGeneratedHeaderFamilyAll {
		t.Fatalf("ARM generated header families = %#v, want one all family", metadata.GeneratedHeaderFamilies)
	}
	family := metadata.GeneratedHeaderFamilies[0]
	inputs, err := metadata.expandedSourceInputGroup(family.SourceInputGroup, "ARM generated headers")
	if err != nil {
		t.Fatalf("expand ARM generated-header inputs: %v", err)
	}
	if got := sourceInputByPath(inputs, "arch/arm/tools/syscall.tbl").Path; got == "" {
		t.Fatalf("ARM generated-header inputs = %v, want arch/arm/tools/syscall.tbl", inputs)
	}
	config := configByName(metadata, "arm")
	variant := variantByTarget(metadata, objectTarget(metadata, config, "arch/arm/kernel/entry-common.o"))
	var environment CompactCompileEnvironment
	for _, candidate := range metadata.CompileEnvironments {
		if candidate.ID == variant.CompileEnvironment {
			environment = candidate
			break
		}
	}
	if want := []string{family.ID}; !reflect.DeepEqual(environment.GeneratedHeaderFamilies, want) {
		t.Fatalf("ARM entry-common generated-header families = %v, want %v", environment.GeneratedHeaderFamilies, want)
	}
}

func TestCompactMetadataBatchEmitsConfigGraphs(t *testing.T) {
	tree := mustParseCompactFixture(t)
	parseKbuild := func(name, content string) *KbuildFile {
		t.Helper()
		kb, err := ParseKbuild(strings.NewReader(content), name)
		if err != nil {
			t.Fatalf("ParseKbuild(%s) failed: %v", name, err)
		}
		return kb
	}
	baseKbuild := parseKbuild("base/Kbuild", "obj-y += init.o\n")
	debugKbuild := parseKbuild("debug/Kbuild", "obj-y += init.o debug.o\n")
	sourceRoot := t.TempDir()
	writeCompactSource(t, sourceRoot, "init.c")
	writeCompactSource(t, sourceRoot, "debug.c")
	writeCompactContentGraphForcedInputs(t, sourceRoot)

	configs := []NamedConfig{
		{Name: "debug", Flags: map[string]string{"CONFIG_DEBUG": "y"}},
		{Name: "base"},
	}
	opts := CompactMetadataOptions{
		SourceRoot:            sourceRoot,
		Srcarch:               "x86",
		CompileEnvironmentABI: "object-abi-v1",
	}
	labels := map[string]string{
		"base":  "//headers:base",
		"debug": "//headers:debug",
	}
	kbuilds := map[string]*KbuildFile{
		"base":  baseKbuild,
		"debug": debugKbuild,
	}

	var calls []string
	batch, err := tree.CompactMetadataBatchWithOptions(configs, opts, func(config *ResolvedConfig) (CompactConfigGraph, error) {
		calls = append(calls, config.Name)
		return CompactConfigGraph{
			Kbuild:                kbuilds[config.Name],
			GeneratedHeadersLabel: labels[config.Name],
		}, nil
	})
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}
	if want := []string{"debug", "base"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("Kbuild callback calls = %v, want %v", calls, want)
	}
	if got := len(batch.Configs); got != len(configs) {
		t.Fatalf("batch config count = %d, want %d", got, len(configs))
	}
	if got, want := batch.GeneratedHeaderFamilies[0].Labels, []string{"//headers:base", "//headers:debug"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("batch header labels = %v, want %v", got, want)
	}

	t.Run("nil resolver", func(t *testing.T) {
		_, err := tree.CompactMetadataBatchWithOptions(configs[:1], opts, nil)
		if err == nil || !strings.Contains(err.Error(), "config graph resolver must not be nil") {
			t.Fatalf("CompactMetadataBatchWithOptions() error = %v", err)
		}
	})
	t.Run("callback error", func(t *testing.T) {
		_, err := tree.CompactMetadataBatchWithOptions(configs[:1], opts, func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{}, fmt.Errorf("sentinel")
		})
		if err == nil || !strings.Contains(err.Error(), `resolve Kbuild for config "debug": sentinel`) {
			t.Fatalf("CompactMetadataBatchWithOptions() error = %v", err)
		}
	})
	t.Run("nil Kbuild", func(t *testing.T) {
		_, err := tree.CompactMetadataBatchWithOptions(configs[:1], opts, func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{}, nil
		})
		if err == nil || !strings.Contains(err.Error(), `resolve Kbuild for config "debug": nil Kbuild`) {
			t.Fatalf("CompactMetadataBatchWithOptions() error = %v", err)
		}
	})
	for _, label := range []string{"", " \t"} {
		t.Run("empty generated headers label", func(t *testing.T) {
			_, err := tree.CompactMetadataBatchWithOptions(configs[:1], opts, func(*ResolvedConfig) (CompactConfigGraph, error) {
				return CompactConfigGraph{
					Kbuild:                debugKbuild,
					GeneratedHeadersLabel: label,
				}, nil
			})
			if err == nil || !strings.Contains(err.Error(), `resolve Kbuild for config "debug": generated headers label must not be empty`) {
				t.Fatalf("CompactMetadataBatchWithOptions() error = %v", err)
			}
		})
	}
}

func TestCompactContentGraphContentIDsTrackOnlyTransitiveInputs(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader("obj-y += init.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "init.c", "#include \"shared.h\"\n")
	mustWriteSource(t, sourceRoot, "shared.h", "#define VALUE 1\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	generate := func() CompactObjectVariant {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
		}
		config := configByName(metadata, "base")
		return variantByTarget(metadata, objectTarget(metadata, config, "init.o"))
	}

	before := generate()
	mustWriteSource(t, sourceRoot, "unrelated.h", "#define UNRELATED 1\n")
	unrelated := generate()
	if unrelated.ContentID != before.ContentID || unrelated.Target != before.Target {
		t.Fatalf("unrelated source changed init identity: before=%q after=%q", before.ContentID, unrelated.ContentID)
	}
	mustWriteSource(t, sourceRoot, "shared.h", "#define VALUE 2\n")
	changed := generate()
	if changed.ContentID == before.ContentID || changed.Target == before.Target {
		t.Fatalf("transitive source change did not change init identity: before=%q after=%q", before.ContentID, changed.ContentID)
	}
}

func TestCompactContentGraphValidationRecomputesContentIDs(t *testing.T) {
	generate := func(t *testing.T) *CompactMetadata {
		t.Helper()
		tree := mustParseCompactFixture(t)
		kb, err := ParseKbuild(strings.NewReader("obj-y += init.o\n"), "Kbuild")
		if err != nil {
			t.Fatalf("ParseKbuild() failed: %v", err)
		}
		sourceRoot := t.TempDir()
		mustWriteSource(t, sourceRoot, "init.c", "int init_value;\n")
		writeCompactContentGraphForcedInputs(t, sourceRoot)
		metadata, err := tree.CompactMetadataBatchWithOptions([]NamedConfig{{Name: "base"}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "x86",
			CompileEnvironmentABI: "object-abi-v1",
		}, func(*ResolvedConfig) (CompactConfigGraph, error) {
			return CompactConfigGraph{
				Kbuild:                kb,
				GeneratedHeadersLabel: "//headers:test",
			}, nil
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
		}
		return metadata
	}
	mutateDigest := func(t *testing.T, metadata *CompactMetadata, path string) {
		t.Helper()
		index, err := metadata.sourceFileIndex(path)
		if err != nil {
			t.Fatal(err)
		}
		metadata.SourceFiles[index-1].Digest = strings.Repeat("f", 64)
	}
	assertRejected := func(t *testing.T, metadata *CompactMetadata, want string) {
		t.Helper()
		err := metadata.validateContentIDs()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("validateContentIDs() error = %v, want %q", err, want)
		}
	}

	t.Run("payload content", func(t *testing.T) {
		metadata := generate(t)
		metadata.ConfigPayloads[0].Content += "CONFIG_MUTATED=y\n"
		assertRejected(t, metadata, "config payload")
	})
	t.Run("header source digest", func(t *testing.T) {
		metadata := generate(t)
		var family CompactGeneratedHeaderFamily
		for _, candidate := range metadata.GeneratedHeaderFamilies {
			if candidate.SourceInputGroup != 0 {
				family = candidate
				break
			}
		}
		inputs, err := metadata.expandedSourceInputGroup(
			family.SourceInputGroup,
			"generated header family",
		)
		if err != nil {
			t.Fatal(err)
		}
		mutateDigest(t, metadata, inputs[0].Path)
		assertRejected(t, metadata, "generated header family")
	})
	t.Run("empty header label", func(t *testing.T) {
		metadata := generate(t)
		metadata.GeneratedHeaderFamilies[0].Labels[0] = ""
		assertRejected(t, metadata, "empty label")
	})
	t.Run("compile ABI", func(t *testing.T) {
		metadata := generate(t)
		metadata.CompileEnvironments[0].ABI += "-mutated"
		assertRejected(t, metadata, "compile environment")
	})
	t.Run("leaf flags", func(t *testing.T) {
		metadata := generate(t)
		metadata.ObjectVariants[0].Flags = append(metadata.ObjectVariants[0].Flags, "-DMUTATED")
		assertRejected(t, metadata, "object target")
	})
	t.Run("leaf source digest", func(t *testing.T) {
		metadata := generate(t)
		mutateDigest(t, metadata, "init.c")
		assertRejected(t, metadata, "object target")
	})
	t.Run("module root", func(t *testing.T) {
		metadata := generate(t)
		metadata.ObjectVariants[0].ModuleRoot = true
		assertRejected(t, metadata, "object target")
	})
	t.Run("objtool disabled", func(t *testing.T) {
		metadata := generate(t)
		metadata.ObjectVariants[0].ObjtoolDisabled = true
		assertRejected(t, metadata, "object target")
	})
	t.Run("objtool force", func(t *testing.T) {
		metadata := generate(t)
		metadata.ObjectVariants[0].ObjtoolForce = true
		assertRejected(t, metadata, "object target")
	})
	t.Run("objtool args", func(t *testing.T) {
		metadata := generate(t)
		metadata.ObjectVariants[0].ObjtoolArgs = []string{"--mutated"}
		assertRejected(t, metadata, "object target")
	})
	t.Run("dependency order", func(t *testing.T) {
		metadata := generate(t)
		root := metadata.ObjectVariants[0]
		inputs, err := metadata.expandedSourceInputGroup(root.SourceInputGroup, "root")
		if err != nil {
			t.Fatal(err)
		}
		abi := metadata.CompileEnvironments[0].ABI
		dependencies := make([]CompactObjectVariant, 0, 2)
		for _, object := range []string{"deps/a.o", "deps/b.o"} {
			dependency := root
			dependency.Object = object
			dependency.Deps = nil
			dependency.ContentID = objectVariantContentID(
				dependency.Object,
				dependency.Mode,
				dependency.ModName,
				dependency.Flags,
				dependency.RemoveFlags,
				dependency.CompileEnvironment,
				dependency.Source,
				inputs,
				nil,
				nil,
				abi,
				false,
				false,
				false,
				nil,
			)
			dependency.Target = sanitizeTargetName(strings.TrimSuffix(object, ".o")) + "__" + compactShortID(dependency.ContentID)
			dependencies = append(dependencies, dependency)
		}
		metadata.ObjectVariants = append(metadata.ObjectVariants, dependencies...)
		dependencyTargets := []string{dependencies[0].Target, dependencies[1].Target}
		dependencyIDs := []string{dependencies[0].ContentID, dependencies[1].ContentID}
		slices.Sort(dependencyTargets)
		slices.Sort(dependencyIDs)
		root = metadata.ObjectVariants[0]
		root.Deps = dependencyTargets
		root.ContentID = objectVariantContentID(
			root.Object,
			root.Mode,
			root.ModName,
			root.Flags,
			root.RemoveFlags,
			root.CompileEnvironment,
			root.Source,
			inputs,
			dependencyIDs,
			nil,
			abi,
			root.ModuleRoot,
			root.ObjtoolDisabled,
			root.ObjtoolForce,
			root.ObjtoolArgs,
		)
		root.Target = sanitizeTargetName(strings.TrimSuffix(root.Object, ".o")) + "__" + compactShortID(root.ContentID)
		metadata.ObjectVariants[0] = root
		if err := metadata.validateContentIDs(); err != nil {
			t.Fatalf("validateContentIDs() rejected canonical dependencies: %v", err)
		}
		metadata.ObjectVariants[0].Deps[0], metadata.ObjectVariants[0].Deps[1] =
			metadata.ObjectVariants[0].Deps[1], metadata.ObjectVariants[0].Deps[0]
		assertRejected(t, metadata, "non-canonical dependencies")
	})
}

func TestCompactContentGraphSourceAndKernelFlagConfigsSplitCompileIdentity(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "Exact compile identity"

config SOURCE_GATE
	bool "Source gate"

config CC_OPTIMIZE_FOR_SIZE
	bool "Optimize for size"
`)
	kb, err := ParseKbuild(strings.NewReader("obj-y += init.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "init.c", `
#if defined(CONFIG_SOURCE_GATE)
int source_gate;
#endif
`)
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{
		{Name: "base"},
		{Name: "source", Flags: map[string]string{"CONFIG_SOURCE_GATE": "y"}},
		{Name: "opt", Flags: map[string]string{"CONFIG_CC_OPTIMIZE_FOR_SIZE": "y"}},
	}, CompactMetadataOptions{
		SourceRoot:            sourceRoot,
		CompileEnvironmentABI: "object-abi-v1",
	})
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}

	variants := map[string]CompactObjectVariant{}
	for _, name := range []string{"base", "source", "opt"} {
		config := configByName(metadata, name)
		variants[name] = variantByTarget(metadata, objectTarget(metadata, config, "init.o"))
	}
	if got := variants["source"].configFragment["CONFIG_SOURCE_GATE"]; got != "y" {
		t.Fatalf("source payload CONFIG_SOURCE_GATE = %q, want y", got)
	}
	if got := variants["opt"].configFragment["CONFIG_CC_OPTIMIZE_FOR_SIZE"]; got != "y" {
		t.Fatalf("optimization payload CONFIG_CC_OPTIMIZE_FOR_SIZE = %q, want y", got)
	}
	for _, name := range []string{"source", "opt"} {
		if variants[name].CompileEnvironment == variants["base"].CompileEnvironment {
			t.Errorf("%s config did not change the compile environment", name)
		}
		if variants[name].ContentID == variants["base"].ContentID {
			t.Errorf("%s config did not change the object content ID", name)
		}
	}
}

func TestCompactContentGraphGeneratedObjectActionFootprints(t *testing.T) {
	tests := []struct {
		object          string
		wantInput       string
		wantClosure     string
		wantInclude     string
		wantConfig      string
		additionalFlags []string
	}{
		{"drivers/tty/vt/ucs.o", "drivers/tty/vt/ucs_width_table.h_shipped", "", "ucs_width_table.h", "", nil},
		{"drivers/scsi/scsi_sysfs.o", "include/scsi/scsi_devinfo.h", "", "scsi_devinfo_tbl.c", "", nil},
		{"drivers/tty/vt/consolemap_deftbl.o", "", "include/linux/types.h", "", "", nil},
		{"lib/crc/crc32-main.o", "", "", "crc32table.h", "", nil},
		{"lib/crc32.o", "", "", "crc32table.h", "CONFIG_CRC32_SLICEBY4", nil},
		{"lib/crc/crc64-main.o", "", "", "crc64table.h", "", nil},
		{"lib/oid_registry.o", "include/linux/oid_registry.h", "", "oid_registry_data.c", "", nil},
		{"arch/x86/lib/inat.o", "arch/x86/lib/x86-opcode-map.txt", "", "inat-tables.c", "", nil},
		{"usr/initramfs_data.o", "usr/default_cpio_list", "", "", "", nil},
		{"arch/x86/kernel/cpu/capflags.o", "", "arch/x86/include/asm/cpufeatures.h", "", "", nil},
		{"arch/x86/realmode/rmpiggy.o", "", "", "pasyms.h", "", nil},
		{"init/version.o", "init/version-timestamp.c", "", "", "", nil},
		{"lib/fdt_ro.o", "scripts/dtc/libfdt/fdt_ro.c", "", "", "", nil},
		{"crypto/example.asn1.o", "scripts/asn1_compiler.c", "", "", "", nil},
		{"init/uts.o", "", "", "", "CONFIG_LOCALVERSION", []string{"-include", "$(obj)/utsversion-tmp.h"}},
	}
	for _, tc := range tests {
		t.Run(tc.object, func(t *testing.T) {
			got := compactObjectActionFootprintForObject(tc.object, tc.additionalFlags)
			if tc.wantInput != "" && !slices.Contains(got.sourceInputs, tc.wantInput) {
				t.Errorf("source inputs = %v, want %q", got.sourceInputs, tc.wantInput)
			}
			if tc.wantClosure != "" && !slices.Contains(got.closureInputs, tc.wantClosure) {
				t.Errorf("closure inputs = %v, want %q", got.closureInputs, tc.wantClosure)
			}
			if tc.wantInclude != "" && !slices.Contains(got.providedIncludes, tc.wantInclude) {
				t.Errorf("provided includes = %v, want %q", got.providedIncludes, tc.wantInclude)
			}
			if tc.wantConfig != "" && !slices.Contains(got.configSymbols, tc.wantConfig) {
				t.Errorf("config symbols = %v, want %q", got.configSymbols, tc.wantConfig)
			}
		})
	}
	for object, want := range map[string][]string{
		"arch/arm/vdso/vdso.o": {
			"arch/arm/vdso/vdso.so",
		},
		"arch/arm64/kernel/vdso-wrap.o": {
			"arch/arm64/kernel/vdso/vdso.so",
		},
		"arch/arm64/kernel/vdso32-wrap.o": {
			"arch/arm64/kernel/vdso32/vdso.so",
		},
		"arch/riscv/kernel/vdso/vdso.o": {
			"arch/riscv/kernel/vdso/vdso.so",
		},
		"arch/riscv/kernel/compat_vdso/compat_vdso.o": {
			"arch/riscv/kernel/compat_vdso/compat_vdso.so",
		},
		"arch/powerpc/kernel/vdso64_wrapper.o": {
			"arch/powerpc/kernel/vdso/vdso64.so.dbg",
		},
		"arch/powerpc/kernel/vdso32_wrapper.o": {
			"arch/powerpc/kernel/vdso/vdso32.so.dbg",
		},
		"arch/x86/purgatory/kexec-purgatory.o": {
			"arch/x86/purgatory/purgatory.ro",
		},
		"arch/riscv/purgatory/kexec-purgatory.o": {
			"arch/riscv/purgatory/purgatory.ro",
		},
		"arch/powerpc/purgatory/kexec-purgatory.o": {
			"arch/powerpc/purgatory/purgatory.ro",
		},
		"arch/x86/realmode/rmpiggy.o": {
			"arch/x86/realmode/rm/realmode.bin",
			"arch/x86/realmode/rm/realmode.relocs",
		},
		"usr/initramfs_data.o": {
			"usr/initramfs_inc_data",
		},
	} {
		got := compactObjectActionFootprintForObject(object, nil)
		for _, input := range want {
			if !slices.Contains(got.providedIncludes, input) {
				t.Errorf("%s provided action inputs = %v, want %q", object, got.providedIncludes, input)
			}
		}
	}
}

func TestCompactContentGraphPowerPCVDSOWrappersBindDebugImages(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"PowerPC vDSO debug-image identity\"\n")
	kb, err := ParseKbuild(strings.NewReader(`
obj-y := arch/powerpc/kernel/vdso64_wrapper.o
obj-y += arch/powerpc/kernel/vdso32_wrapper.o
`), "Makefile")
	if err != nil {
		t.Fatalf("parseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	for path, content := range map[string]string{
		"arch/powerpc/kernel/vdso64_wrapper.S":         ".incbin \"arch/powerpc/kernel/vdso/vdso64.so.dbg\"\n",
		"arch/powerpc/kernel/vdso32_wrapper.S":         ".incbin \"arch/powerpc/kernel/vdso/vdso32.so.dbg\"\n",
		"arch/powerpc/kernel/vdso/cacheflush.S":        "nop\n",
		"arch/powerpc/kernel/vdso/datapage.S":          "nop\n",
		"arch/powerpc/kernel/vdso/getcpu.S":            "nop\n",
		"arch/powerpc/kernel/vdso/getrandom.S":         "nop\n",
		"arch/powerpc/kernel/vdso/gettimeofday.S":      "nop\n",
		"arch/powerpc/kernel/vdso/note.S":              "nop\n",
		"arch/powerpc/kernel/vdso/vgetrandom-chacha.S": "nop\n",
		"arch/powerpc/kernel/vdso/vgetrandom.c":        "int ppc_vgetrandom;\n",
		"arch/powerpc/kernel/vdso/vgettimeofday.c":     "int ppc_vgettimeofday_v1;\n",
		"arch/powerpc/kernel/vdso/sigtramp64.S":        "nop\n",
		"arch/powerpc/kernel/vdso/vdso64.lds.S":        "SECTIONS { .text : { *(.text*) } }\n",
		"arch/powerpc/kernel/vdso/sigtramp32.S":        "nop\n",
		"arch/powerpc/kernel/vdso/vdso32.lds.S":        "SECTIONS { .text : { *(.text*) } }\n",
		"arch/powerpc/lib/crtsavres.S":                 "nop\n",
		"lib/vdso/getrandom.c":                         "int generic_getrandom;\n",
		"lib/vdso/gettimeofday.c":                      "int generic_gettimeofday;\n",
	} {
		mustWriteSource(t, sourceRoot, path, content)
	}
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	generate := func(name string) (*CompactMetadata, map[string]CompactObjectVariant) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: name}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "powerpc",
			CompileEnvironmentABI: "powerpc-vdso-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		return metadata, map[string]CompactObjectVariant{
			"64": variantByTarget(metadata, objectTarget(metadata, config, "arch/powerpc/kernel/vdso64_wrapper.o")),
			"32": variantByTarget(metadata, objectTarget(metadata, config, "arch/powerpc/kernel/vdso32_wrapper.o")),
		}
	}

	metadata, before := generate("before")
	if len(metadata.GeneratedHeaderFamilies) != 1 ||
		metadata.GeneratedHeaderFamilies[0].Name != compactGeneratedHeaderFamilyAll {
		t.Fatalf("PowerPC vDSO generated-header families = %#v, want one all family", metadata.GeneratedHeaderFamilies)
	}
	family := metadata.GeneratedHeaderFamilies[0]
	inputs, err := metadata.expandedSourceInputGroup(family.SourceInputGroup, "PowerPC vDSO producers")
	if err != nil {
		t.Fatal(err)
	}
	paths := sourceInputPaths(inputs)
	for _, want := range []string{
		"arch/powerpc/kernel/vdso/vgettimeofday.c",
		"arch/powerpc/kernel/vdso/vdso64.lds.S",
		"arch/powerpc/kernel/vdso/vdso32.lds.S",
		"arch/powerpc/lib/crtsavres.S",
		"lib/vdso/gettimeofday.c",
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("PowerPC vDSO producer inputs = %v, want %q", paths, want)
		}
	}
	for bits, variant := range before {
		var environment CompactCompileEnvironment
		for _, candidate := range metadata.CompileEnvironments {
			if candidate.ID == variant.CompileEnvironment {
				environment = candidate
				break
			}
		}
		if !slices.Contains(environment.GeneratedHeaderFamilies, family.ID) {
			t.Errorf("PowerPC %s-bit wrapper does not bind generated family %q", bits, family.ID)
		}
	}

	mustWriteSource(t, sourceRoot, "arch/powerpc/kernel/vdso/vgettimeofday.c", "int ppc_vgettimeofday_v2;\n")
	changedMetadata, changed := generate("changed")
	if changedMetadata.GeneratedHeaderFamilies[0].ID == family.ID {
		t.Fatalf("PowerPC vDSO producer source did not change generated family %q", family.ID)
	}
	for bits := range before {
		if changed[bits].ContentID == before[bits].ContentID {
			t.Errorf("PowerPC %s-bit wrapper content ID did not change with producer", bits)
		}
	}
}

func TestCompactContentGraphPowerPCVDSOFamilyBindsMakefileSelection(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "PowerPC vDSO Makefile selection"

config PPC64
	bool "64-bit PowerPC"

config VDSO32
	bool "32-bit vDSO"

config GENERIC_GETTIMEOFDAY
	bool "generic gettimeofday"

config VDSO_GETRANDOM
	bool "vDSO getrandom"
`)
	kb, err := ParseKbuild(strings.NewReader("obj-y := init/main.o\n"), "Makefile")
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "init/main.c", "int main_v1;\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	familyID := func(name string, flags map[string]string) string {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: name, Flags: flags}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "powerpc",
			CompileEnvironmentABI: "powerpc-vdso-config-abi-v1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(metadata.GeneratedHeaderFamilies) != 1 {
			t.Fatalf("generated-header families = %#v, want one all family", metadata.GeneratedHeaderFamilies)
		}
		return metadata.GeneratedHeaderFamilies[0].ID
	}

	base := familyID("base", nil)
	for _, symbol := range []string{
		"CONFIG_PPC64",
		"CONFIG_VDSO32",
		"CONFIG_GENERIC_GETTIMEOFDAY",
		"CONFIG_VDSO_GETRANDOM",
	} {
		if got := familyID(symbol, map[string]string{symbol: "y"}); got == base {
			t.Errorf("%s did not change PowerPC generated-family identity %q", symbol, base)
		}
	}
}

func TestCompactContentGraphRISCVVDSOWrappersBindExactGeneratedBinaries(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"RISC-V vDSO exact identity\"\n")
	kb, err := ParseKbuild(strings.NewReader(`
obj-y := arch/riscv/kernel/vdso/vdso.o
obj-y += arch/riscv/kernel/compat_vdso/compat_vdso.o
`), "Makefile")
	if err != nil {
		t.Fatalf("parseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	for path, content := range map[string]string{
		"arch/riscv/kernel/vdso/vdso.S":                   ".incbin __VDSO_PATH\n",
		"arch/riscv/kernel/compat_vdso/compat_vdso.S":     "#define __VDSO_PATH \"arch/riscv/kernel/compat_vdso/compat_vdso.so\"\n#include \"../vdso/vdso.S\"\n",
		"arch/riscv/kernel/vdso/flush_icache.S":           "nop\n",
		"arch/riscv/kernel/vdso/getcpu.S":                 "nop\n",
		"arch/riscv/kernel/vdso/getrandom.c":              "int getrandom;\n",
		"arch/riscv/kernel/vdso/hwprobe.c":                "int hwprobe;\n",
		"arch/riscv/kernel/vdso/note.S":                   "nop\n",
		"arch/riscv/kernel/vdso/rt_sigreturn.S":           "nop\n",
		"arch/riscv/kernel/vdso/sys_hwprobe.S":            "nop\n",
		"arch/riscv/kernel/vdso/vdso.lds.S":               "SECTIONS { .text : { *(.text*) } }\n",
		"arch/riscv/kernel/vdso/vgetrandom-chacha.S":      "nop\n",
		"arch/riscv/kernel/vdso/vgettimeofday.c":          "int gettimeofday;\n",
		"arch/riscv/kernel/compat_vdso/compat_vdso.lds.S": "SECTIONS { .text : { *(.text*) } }\n",
		"arch/riscv/kernel/compat_vdso/flush_icache.S":    "nop\n",
		"arch/riscv/kernel/compat_vdso/getcpu.S":          "nop\n",
		"arch/riscv/kernel/compat_vdso/note.S":            "nop\n",
		"arch/riscv/kernel/compat_vdso/rt_sigreturn.S":    "nop\n",
		"lib/vdso/getrandom.c":                            "int generic_getrandom;\n",
		"lib/vdso/gettimeofday.c":                         "int generic_gettimeofday;\n",
	} {
		mustWriteSource(t, sourceRoot, path, content)
	}
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	generate := func(name string) (*CompactMetadata, map[string]CompactObjectVariant) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: name}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "riscv",
			CompileEnvironmentABI: "riscv-object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		return metadata, map[string]CompactObjectVariant{
			"native": variantByTarget(metadata, objectTarget(metadata, config, "arch/riscv/kernel/vdso/vdso.o")),
			"compat": variantByTarget(metadata, objectTarget(metadata, config, "arch/riscv/kernel/compat_vdso/compat_vdso.o")),
		}
	}

	metadata, before := generate("before")
	if len(metadata.GeneratedHeaderFamilies) != 1 ||
		metadata.GeneratedHeaderFamilies[0].Name != compactGeneratedHeaderFamilyAll {
		t.Fatalf("RISC-V vDSO generated-header families = %#v, want one all family", metadata.GeneratedHeaderFamilies)
	}
	family := metadata.GeneratedHeaderFamilies[0]
	familyInputs, err := metadata.expandedSourceInputGroup(family.SourceInputGroup, "RISC-V generated headers")
	if err != nil {
		t.Fatal(err)
	}
	paths := sourceInputPaths(familyInputs)
	for _, want := range []string{
		"arch/riscv/kernel/vdso/hwprobe.c",
		"arch/riscv/kernel/vdso/vdso.lds.S",
		"arch/riscv/kernel/compat_vdso/compat_vdso.lds.S",
		"lib/vdso/gettimeofday.c",
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("RISC-V vDSO producer inputs = %v, want %q", paths, want)
		}
	}
	for name, variant := range before {
		var environment CompactCompileEnvironment
		for _, candidate := range metadata.CompileEnvironments {
			if candidate.ID == variant.CompileEnvironment {
				environment = candidate
				break
			}
		}
		if !slices.Contains(environment.GeneratedHeaderFamilies, family.ID) {
			t.Errorf("%s RISC-V wrapper environment = %#v, want family %q", name, environment, family.ID)
		}
	}

	mustWriteSource(t, sourceRoot, "arch/riscv/kernel/vdso/hwprobe.c", "int hwprobe_changed;\n")
	changedMetadata, changed := generate("changed")
	if changedMetadata.GeneratedHeaderFamilies[0].ID == family.ID {
		t.Fatalf("RISC-V vDSO producer source did not change generated-header identity %q", family.ID)
	}
	for name := range before {
		if changed[name].ContentID == before[name].ContentID {
			t.Errorf("%s RISC-V wrapper content ID did not change with producer", name)
		}
	}
}

func TestCompactContentGraphRISCVPurgatoryBindsGeneratedImageAndProducers(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "RISC-V purgatory image identity"

config KASAN_GENERIC
	bool "generic KASAN"

config KASAN_SW_TAGS
	bool "software-tag KASAN"
`)
	kb, err := ParseKbuild(strings.NewReader(
		"obj-y := arch/riscv/purgatory/kexec-purgatory.o\n",
	), "Makefile")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	for path, content := range map[string]string{
		"arch/riscv/purgatory/kexec-purgatory.S": ".incbin \"arch/riscv/purgatory/purgatory.ro\"\n",
		"arch/riscv/purgatory/purgatory.c":       "int purgatory;\n",
		"arch/riscv/purgatory/entry.S":           "nop\n",
		"lib/crypto/sha256.c":                    "int sha256;\n",
		"lib/string.c":                           "int string;\n",
		"lib/ctype.c":                            "int ctype;\n",
		"arch/riscv/lib/memcpy.S":                "nop\n",
		"arch/riscv/lib/memset.S":                "nop\n",
		"arch/riscv/lib/strcmp.S":                "nop\n",
		"arch/riscv/lib/strlen.S":                "nop\n",
		"arch/riscv/lib/strncmp.S":               "nop\n",
	} {
		mustWriteSource(t, sourceRoot, path, content)
	}
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	generate := func(name string, flags map[string]string) (*CompactMetadata, CompactObjectVariant) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: name, Flags: flags}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "riscv",
			CompileEnvironmentABI: "riscv-purgatory-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		return metadata, variantByTarget(metadata, objectTarget(metadata, config, "arch/riscv/purgatory/kexec-purgatory.o"))
	}

	metadata, before := generate("before", nil)
	if len(metadata.GeneratedHeaderFamilies) != 1 ||
		metadata.GeneratedHeaderFamilies[0].Name != compactGeneratedHeaderFamilyAll {
		t.Fatalf("RISC-V purgatory generated-header families = %#v, want one all family", metadata.GeneratedHeaderFamilies)
	}
	family := metadata.GeneratedHeaderFamilies[0]
	inputs, err := metadata.expandedSourceInputGroup(family.SourceInputGroup, "RISC-V purgatory producers")
	if err != nil {
		t.Fatal(err)
	}
	paths := sourceInputPaths(inputs)
	for _, want := range []string{
		"arch/riscv/purgatory/purgatory.c",
		"arch/riscv/purgatory/entry.S",
		"lib/crypto/sha256.c",
		"lib/string.c",
		"lib/ctype.c",
		"arch/riscv/lib/memcpy.S",
		"arch/riscv/lib/memset.S",
		"arch/riscv/lib/strcmp.S",
		"arch/riscv/lib/strlen.S",
		"arch/riscv/lib/strncmp.S",
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("RISC-V purgatory producer inputs = %v, want %q", paths, want)
		}
	}
	var environment CompactCompileEnvironment
	for _, candidate := range metadata.CompileEnvironments {
		if candidate.ID == before.CompileEnvironment {
			environment = candidate
			break
		}
	}
	if !slices.Contains(environment.GeneratedHeaderFamilies, family.ID) {
		t.Fatalf("RISC-V purgatory environment = %#v, want family %q", environment, family.ID)
	}
	kasanMetadata, _ := generate("kasan", map[string]string{"CONFIG_KASAN_GENERIC": "y"})
	if kasanMetadata.GeneratedHeaderFamilies[0].ID == family.ID {
		t.Fatalf("RISC-V KASAN membership did not change generated purgatory identity %q", family.ID)
	}

	mustWriteSource(t, sourceRoot, "lib/crypto/sha256.c", "int sha256_changed;\n")
	changedMetadata, changed := generate("changed", nil)
	if changedMetadata.GeneratedHeaderFamilies[0].ID == family.ID {
		t.Fatalf("RISC-V purgatory producer did not change generated image identity %q", family.ID)
	}
	if changed.ContentID == before.ContentID {
		t.Fatalf("RISC-V purgatory wrapper content ID did not change with producer %q", before.ContentID)
	}
}

func TestCompactContentGraphPowerPCPurgatoryBindsGeneratedImageAndProducer(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "PowerPC purgatory image identity"

config PPC64
	bool "64-bit PowerPC"

config KEXEC_FILE
	bool "file-based kexec"
`)
	kb, err := ParseKbuild(strings.NewReader(
		"obj-$(CONFIG_KEXEC_FILE) := arch/powerpc/purgatory/kexec-purgatory.o\n",
	), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	for path, content := range map[string]string{
		"arch/powerpc/purgatory/kexec-purgatory.S": ".incbin \"arch/powerpc/purgatory/purgatory.ro\"\n",
		"arch/powerpc/purgatory/trampoline_64.S":   "nop\n",
	} {
		mustWriteSource(t, sourceRoot, path, content)
	}
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	generate := func(name string, flags map[string]string) (*CompactMetadata, CompactObjectVariant) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: name, Flags: flags}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "powerpc",
			CompileEnvironmentABI: "powerpc-purgatory-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		return metadata, variantByTarget(metadata, objectTarget(metadata, config, "arch/powerpc/purgatory/kexec-purgatory.o"))
	}

	offMetadata, _ := generate("off", map[string]string{"CONFIG_PPC64": "y"})
	if target := objectTarget(offMetadata, configByName(offMetadata, "off"), "arch/powerpc/purgatory/kexec-purgatory.o"); target != "" {
		t.Fatalf("PowerPC purgatory target %q exists without CONFIG_KEXEC_FILE", target)
	}
	metadata, before := generate("before", map[string]string{
		"CONFIG_PPC64":      "y",
		"CONFIG_KEXEC_FILE": "y",
	})
	if before.Target == "" {
		t.Fatal("PowerPC purgatory target is absent with CONFIG_KEXEC_FILE=y")
	}
	if len(metadata.GeneratedHeaderFamilies) != 1 ||
		metadata.GeneratedHeaderFamilies[0].Name != compactGeneratedHeaderFamilyAll {
		t.Fatalf("PowerPC purgatory generated-header families = %#v, want one all family", metadata.GeneratedHeaderFamilies)
	}
	family := metadata.GeneratedHeaderFamilies[0]
	inputs, err := metadata.expandedSourceInputGroup(family.SourceInputGroup, "PowerPC purgatory producer")
	if err != nil {
		t.Fatal(err)
	}
	paths := sourceInputPaths(inputs)
	if want := "arch/powerpc/purgatory/trampoline_64.S"; !slices.Contains(paths, want) {
		t.Fatalf("PowerPC purgatory producer inputs = %v, want %q", paths, want)
	}
	var environment CompactCompileEnvironment
	for _, candidate := range metadata.CompileEnvironments {
		if candidate.ID == before.CompileEnvironment {
			environment = candidate
			break
		}
	}
	if !slices.Contains(environment.GeneratedHeaderFamilies, family.ID) {
		t.Fatalf("PowerPC purgatory environment = %#v, want family %q", environment, family.ID)
	}

	mustWriteSource(t, sourceRoot, "arch/powerpc/purgatory/trampoline_64.S", "nop\nnop\n")
	changedMetadata, changed := generate("changed", map[string]string{
		"CONFIG_PPC64":      "y",
		"CONFIG_KEXEC_FILE": "y",
	})
	if changedMetadata.GeneratedHeaderFamilies[0].ID == family.ID {
		t.Fatalf("PowerPC purgatory producer did not change generated image identity %q", family.ID)
	}
	if changed.ContentID == before.ContentID {
		t.Fatalf("PowerPC purgatory wrapper content ID did not change with producer %q", before.ContentID)
	}
}

func TestCompactContentGraphPowerPCArchRootIncludeIsRecursive(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"PowerPC architecture include root\"\n")
	sourceRoot := t.TempDir()
	for path, content := range map[string]string{
		"Kbuild":                       "obj-y += arch/powerpc/kernel/\n",
		"arch/powerpc/Makefile":        "KBUILD_CPPFLAGS += -I $(srctree)/arch/powerpc\n",
		"arch/powerpc/kernel/Makefile": "obj-y += prom.o\n",
		"arch/powerpc/kernel/prom.c":   "#include <mm/mmu_decl.h>\nint powerpc_prom;\n",
		"arch/powerpc/mm/mmu_decl.h":   "#define POWERPC_MMU_DECL 1\n",
	} {
		mustWriteSource(t, sourceRoot, path, content)
	}
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	kb, err := ParseKbuildDirectoryTree(filepath.Join(sourceRoot, "Kbuild"), KbuildOptions{
		RootDir:       sourceRoot,
		RootMakefiles: []string{"arch/powerpc/Makefile"},
		Variables:     map[string]string{"SRCARCH": "powerpc"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	generate := func(name string) (*CompactMetadata, CompactObjectVariant) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: name}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "powerpc",
			CompileEnvironmentABI: "powerpc-object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		return metadata, variantByTarget(metadata, objectTarget(metadata, config, "arch/powerpc/kernel/prom.o"))
	}

	metadata, before := generate("before")
	if !reflect.DeepEqual(before.Flags, []string{"-I", "$(srctree)/arch/powerpc"}) {
		t.Fatalf("PowerPC prom flags = %#v, want recursive architecture include root", before.Flags)
	}
	inputs, err := metadata.expandedSourceInputGroup(before.SourceInputGroup, "PowerPC prom")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sourceInputPaths(inputs), "arch/powerpc/mm/mmu_decl.h") {
		t.Fatalf("PowerPC prom inputs = %v, want arch-relative mmu_decl.h", sourceInputPaths(inputs))
	}

	mustWriteSource(t, sourceRoot, "arch/powerpc/mm/mmu_decl.h", "#define POWERPC_MMU_DECL 2\n")
	_, changed := generate("changed")
	if changed.ContentID == before.ContentID {
		t.Fatalf("PowerPC arch-relative header did not change prom content ID %q", before.ContentID)
	}
}

func TestCompactContentGraphGeneratedUTSVersionForcedInput(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "generated UTS version input"

config SMP
	bool "SMP"
`)
	kb, err := ParseKbuild(strings.NewReader(`
obj-y := init/version.o
CFLAGS_init/version.o += -include init/utsversion-tmp.h
`), "Makefile")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "init/version.c", "int version;\n")
	mustWriteSource(t, sourceRoot, "init/version-timestamp.c", "int version_timestamp;\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)

	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{
		Name:  "smp",
		Flags: map[string]string{"CONFIG_SMP": "y"},
	}}, CompactMetadataOptions{SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}
	config := configByName(metadata, "smp")
	variant := variantByTarget(metadata, objectTarget(metadata, config, "init/version.o"))
	if got := variant.configFragment["CONFIG_SMP"]; got != "y" {
		t.Fatalf("init/version.o CONFIG_SMP footprint = %q, want y", got)
	}
	inputs, err := metadata.expandedSourceInputGroup(variant.SourceInputGroup, "init/version.o")
	if err != nil {
		t.Fatalf("expand init/version.o source inputs: %v", err)
	}
	if got := sourceInputByPath(inputs, "init/utsversion-tmp.h"); got.Path != "" {
		t.Fatalf("generated UTS version header retained as source input: %#v", got)
	}
	if got := sourceInputByPath(inputs, "init/version-timestamp.c"); got.Path == "" {
		t.Fatalf("init/version.o source inputs omit version-timestamp.c: %v", inputs)
	}
}

func TestCompactContentGraphConsoleMapGeneratedSourceBindsHeaderClosure(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader("obj-y += drivers/tty/vt/consolemap_deftbl.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "drivers/tty/vt/cp437.uni", "0x00 U+0000\n")
	mustWriteSource(t, sourceRoot, "include/linux/types.h", "#include <linux/types-nested.h>\n")
	mustWriteSource(t, sourceRoot, "include/linux/types-nested.h", "#define TYPES_NESTED 1\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)

	generate := func() (CompactObjectVariant, []CompactSourceInput) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "x86",
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
		}
		config := configByName(metadata, "base")
		variant := variantByTarget(metadata, objectTarget(metadata, config, "drivers/tty/vt/consolemap_deftbl.o"))
		inputs, err := metadata.expandedSourceInputGroup(
			variant.SourceInputGroup,
			"consolemap_deftbl",
		)
		if err != nil {
			t.Fatalf("expand consolemap source inputs: %v", err)
		}
		return variant, inputs
	}
	before, inputs := generate()
	var paths []string
	for _, input := range inputs {
		paths = append(paths, input.Path)
	}
	for _, want := range []string{"include/linux/types.h", "include/linux/types-nested.h"} {
		if !slices.Contains(paths, want) {
			t.Errorf("consolemap source inputs = %v, want %q", paths, want)
		}
	}
	beforeNested := sourceInputByPath(inputs, "include/linux/types-nested.h")
	mustWriteSource(t, sourceRoot, "include/linux/types-nested.h", "#define TYPES_NESTED 2\n")
	after, afterInputs := generate()
	afterNested := sourceInputByPath(afterInputs, "include/linux/types-nested.h")
	if beforeNested.Digest == afterNested.Digest {
		t.Fatalf("nested consolemap header digest did not change: %q", beforeNested.Digest)
	}
	if before.ContentID == after.ContentID {
		t.Fatalf("nested consolemap header change did not change content ID %q", before.ContentID)
	}
}

func TestCompactContentGraphEmptyRootDTBWrapperBindsLinkerHeaderClosure(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader("obj-y += drivers/of/empty_root.dtb.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "drivers/of/empty_root.dts", "/dts-v1/; / {};\n")
	mustWriteSource(t, sourceRoot, "include/asm-generic/vmlinux.lds.h", "#include <asm-generic/vmlinux-nested.h>\n")
	mustWriteSource(t, sourceRoot, "include/asm-generic/vmlinux-nested.h", "#define VMLINUX_NESTED 1\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)

	generate := func() (CompactObjectVariant, []CompactSourceInput) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "x86",
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
		}
		config := configByName(metadata, "base")
		variant := variantByTarget(metadata, objectTarget(metadata, config, "drivers/of/empty_root.dtb.o"))
		inputs, err := metadata.expandedSourceInputGroup(
			variant.SourceInputGroup,
			"empty_root.dtb.o",
		)
		if err != nil {
			t.Fatalf("expand empty_root.dtb.o source inputs: %v", err)
		}
		return variant, inputs
	}
	before, beforeInputs := generate()
	for _, path := range []string{
		"drivers/of/empty_root.dts",
		"include/asm-generic/vmlinux.lds.h",
		"include/asm-generic/vmlinux-nested.h",
	} {
		if sourceInputByPath(beforeInputs, path).Path == "" {
			t.Fatalf("empty_root.dtb.o source inputs omit %q: %v", path, beforeInputs)
		}
	}
	beforeNested := sourceInputByPath(beforeInputs, "include/asm-generic/vmlinux-nested.h")
	mustWriteSource(t, sourceRoot, "include/asm-generic/vmlinux-nested.h", "#define VMLINUX_NESTED 2\n")
	after, afterInputs := generate()
	afterNested := sourceInputByPath(afterInputs, "include/asm-generic/vmlinux-nested.h")
	if beforeNested.Digest == afterNested.Digest {
		t.Fatalf("nested linker header digest did not change: %q", beforeNested.Digest)
	}
	if before.ContentID == after.ContentID {
		t.Fatalf("nested linker header change did not change empty_root.dtb.o content ID %q", before.ContentID)
	}
}

func TestCompactContentGraphASN1GeneratedParserBindsEmittedHeaderClosures(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader("obj-y += crypto/example.asn1.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "crypto/example.asn1", "Example ::= INTEGER\n")
	mustWriteSource(t, sourceRoot, "scripts/asn1_compiler.c", "int asn1_compiler;\n")
	mustWriteSource(t, sourceRoot, "include/linux/asn1_ber_bytecode.h", "#define ASN1_BER 1\n")
	mustWriteSource(t, sourceRoot, "include/linux/asn1_decoder.h", "#include <linux/asn1_nested.h>\n")
	mustWriteSource(t, sourceRoot, "include/linux/asn1_nested.h", "#define ASN1_NESTED 1\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)

	generate := func() (CompactObjectVariant, []CompactSourceInput) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "x86",
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
		}
		config := configByName(metadata, "base")
		variant := variantByTarget(metadata, objectTarget(metadata, config, "crypto/example.asn1.o"))
		inputs, err := metadata.expandedSourceInputGroup(
			variant.SourceInputGroup,
			"ASN.1 parser test",
		)
		if err != nil {
			t.Fatalf("expand ASN.1 source inputs failed: %v", err)
		}
		return variant, inputs
	}

	before, beforeInputs := generate()
	for _, path := range []string{
		"include/linux/asn1_ber_bytecode.h",
		"include/linux/asn1_decoder.h",
		"include/linux/asn1_nested.h",
		"scripts/asn1_compiler.c",
	} {
		if !slices.ContainsFunc(beforeInputs, func(input CompactSourceInput) bool {
			return input.Path == path
		}) {
			t.Errorf("ASN.1 parser source inputs omit %q: %v", path, beforeInputs)
		}
	}
	mustWriteSource(t, sourceRoot, "include/linux/asn1_nested.h", "#define ASN1_NESTED 2\n")
	after, _ := generate()
	if before.ContentID == after.ContentID {
		t.Fatalf("transitive ASN.1 decoder header change did not change content ID %q", before.ContentID)
	}
}

func TestCompactContentGraphASN1ConsumerRequiresResolvedParserObject(t *testing.T) {
	tree := mustParseCompactFixture(t)
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "crypto/consumer.c", "#include \"parser.asn1.h\"\n")
	mustWriteSource(t, sourceRoot, "crypto/parser.asn1", "Parser ::= INTEGER\n")
	mustWriteSource(t, sourceRoot, "scripts/asn1_compiler.c", "int asn1_compiler;\n")
	mustWriteSource(t, sourceRoot, "include/linux/asn1_ber_bytecode.h", "#define ASN1_BER 1\n")
	mustWriteSource(t, sourceRoot, "include/linux/asn1_decoder.h", "#define ASN1_DECODER 1\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	opts := CompactMetadataOptions{
		SourceRoot:            sourceRoot,
		Srcarch:               "x86",
		CompileEnvironmentABI: "object-abi-v1",
	}

	withParser, err := ParseKbuild(strings.NewReader(`
obj-y += crypto/consumer.o
obj-y += crypto/parser.asn1.o
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild(with parser) failed: %v", err)
	}
	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, withParser, []NamedConfig{{Name: "base"}}, opts)
	if err != nil {
		t.Fatalf("resolved ASN.1 consumer scan failed: %v", err)
	}
	config := configByName(metadata, "base")
	consumer := variantByTarget(metadata, objectTarget(metadata, config, "crypto/consumer.o"))
	if len(consumer.Deps) != 1 {
		t.Fatalf("ASN.1 consumer deps = %v, want one generated parser dependency", consumer.Deps)
	}

	withoutParser, err := ParseKbuild(strings.NewReader("obj-y += crypto/consumer.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild(without parser) failed: %v", err)
	}
	_, err = compactMetadataBatchWithOptionsForTest(t, tree, withoutParser, []NamedConfig{{Name: "base"}}, opts)
	if err == nil || !strings.Contains(err.Error(), "unresolved potentially-active literal include") {
		t.Fatalf("consumer without ASN.1 parser error = %v, want unresolved include", err)
	}
}

func TestCompactContentGraphCapflagsIdentityUsesProducerHeaders(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader("obj-y += arch/x86/kernel/cpu/capflags.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "arch/x86/kernel/cpu/mkcapflags.sh", "# nominal source\n")
	mustWriteSource(t, sourceRoot, "arch/x86/include/asm/cpufeatures.h", "#include <asm/required-features.h>\n#define X86_FEATURE_ONE 1\n")
	mustWriteSource(t, sourceRoot, "arch/x86/include/asm/vmxfeatures.h", "#define VMX_FEATURE_ONE 1\n")
	mustWriteSource(t, sourceRoot, "arch/x86/include/asm/required-features.h", "#define REQUIRED_FEATURE_ONE 1\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	generate := func() (CompactObjectVariant, []CompactSourceInput) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "x86",
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
		}
		config := configByName(metadata, "base")
		variant := variantByTarget(metadata, objectTarget(metadata, config, "arch/x86/kernel/cpu/capflags.o"))
		inputs, err := metadata.expandedSourceInputGroup(
			variant.SourceInputGroup,
			"capflags.o",
		)
		if err != nil {
			t.Fatalf("expand capflags.o source inputs: %v", err)
		}
		return variant, inputs
	}
	before, beforeInputs := generate()
	beforeNested := sourceInputByPath(beforeInputs, "arch/x86/include/asm/required-features.h")
	if beforeNested.Path == "" {
		t.Fatalf("capflags source inputs omit required-features.h: %v", beforeInputs)
	}
	mustWriteSource(t, sourceRoot, "arch/x86/include/asm/required-features.h", "#define REQUIRED_FEATURE_TWO 2\n")
	after, afterInputs := generate()
	afterNested := sourceInputByPath(afterInputs, "arch/x86/include/asm/required-features.h")
	if beforeNested.Digest == afterNested.Digest {
		t.Fatalf("nested capflags header digest did not change: %q", beforeNested.Digest)
	}
	if before.ContentID == after.ContentID {
		t.Fatalf("nested capflags header change did not change content ID %q", before.ContentID)
	}
}

func TestObjectVariantContentIDUsesFullChildIDs(t *testing.T) {
	prefix := strings.Repeat("a", compactShortIDLength)
	left := prefix + strings.Repeat("b", 64-compactShortIDLength)
	right := prefix + strings.Repeat("c", 64-compactShortIDLength)
	leftID := objectVariantContentID("composite.o", "y", "", nil, nil, "", "", nil, nil, []string{left}, "abi-v1", false, false, false, nil)
	rightID := objectVariantContentID("composite.o", "y", "", nil, nil, "", "", nil, nil, []string{right}, "abi-v1", false, false, false, nil)
	if leftID == rightID {
		t.Fatalf("full child content IDs with a shared presentation prefix produced the same parent ID %q", leftID)
	}
}

func TestObjectVariantContentIDPreservesCanonicalFraming(t *testing.T) {
	values := []string{
		"object=drivers/example.o",
		"mode=m",
		"modname=example",
		"compile_environment=environment-id",
		"abi=abi-v1",
		"source=drivers/example.c",
		"flag=-Wall",
		"flag=-DVALUE=1",
		"remove_flag=-Werror",
		"source_input=drivers/example.c\x00source-digest",
		"source_input=include/example.h\x00header-digest",
		"dep_content_id=dependency-id",
		"member_content_id=member-id",
	}
	var canonical strings.Builder
	canonical.WriteString(compactObjectContentDomain)
	canonical.WriteByte(0)
	for _, value := range values {
		canonical.WriteString(value)
		canonical.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	want := hex.EncodeToString(sum[:])

	got := objectVariantContentID(
		"drivers/example.o",
		"m",
		"example",
		[]string{"-Wall", "-DVALUE=1"},
		[]string{"-Werror"},
		"environment-id",
		"drivers/example.c",
		[]CompactSourceInput{
			{Path: "drivers/example.c", Digest: "source-digest"},
			{Path: "include/example.h", Digest: "header-digest"},
		},
		[]string{"dependency-id"},
		[]string{"member-id"},
		"abi-v1",
		false,
		false,
		false,
		nil,
	)
	if got != want {
		t.Fatalf("objectVariantContentID() = %q, want canonical hash %q", got, want)
	}
}

func TestCompactContentGraphCompositeIdentityIgnoresNonActionMetadata(t *testing.T) {
	config := &ResolvedConfig{
		Effective: map[string]string{"CONFIG_UNUSED": "y"},
		Written:   map[string]bool{"CONFIG_UNUSED": true},
	}
	variant := func(flag, modname string) CompactObjectVariant {
		t.Helper()
		object := resolvedKbuildObject{
			object:  "drivers/example/bundle.o",
			mode:    "y",
			modname: modname,
			flags: []resolvedKbuildFlag{{
				language: "any",
				values:   []string{flag},
			}},
			footprint: map[string]bool{"CONFIG_UNUSED": true},
		}
		return object.variant(
			config,
			"",
			[]CompactSourceInput{{Path: "ignored.inc", Digest: strings.Repeat("f", 64)}},
			[]string{"member_target"},
			[]string{"ignored_dep"},
			[]string{strings.Repeat("a", 64)},
			[]string{strings.Repeat("b", 64)},
			"",
			nil,
			"linker-abi-v1",
			nil,
			"x86",
		)
	}
	left := variant("-DLEFT", "left")
	right := variant("-DRIGHT", "right")
	if left.ContentID != right.ContentID || left.Target != right.Target || !left.equal(right) {
		t.Fatalf("irrelevant composite metadata split identity:\nleft=%#v\nright=%#v", left, right)
	}
	if len(left.Flags) != 0 || len(left.sourceInputs) != 0 || len(left.Deps) != 0 || left.ModName != "" {
		t.Fatalf("content graph composite retained ignored action metadata: %#v", left)
	}
}

func TestCompactContentGraphArm64NvheIdentityBindsLinkerScriptAndConfig(t *testing.T) {
	tree := mustParseString(t, `
mainmenu "nVHE exact identity"

config NVHE_LAYOUT
	bool "nVHE layout"
`)
	kb, err := ParseKbuild(strings.NewReader(`
obj-y := arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o
arch/arm64/kvm/hyp/nvhe/kvm_nvhe-y := arch/arm64/kvm/hyp/nvhe/member.nvhe.o
`), "Makefile")
	if err != nil {
		t.Fatalf("parseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "arch/arm64/kvm/hyp/nvhe/member.c", "int member;\n")
	mustWriteSource(t, sourceRoot, "arch/arm64/kvm/hyp/nvhe/hyp.lds.S", `
#if defined(CONFIG_NVHE_LAYOUT)
SECTIONS { .text : { *(.text) } }
#else
SECTIONS { .text : { *(.text*) } }
#endif
`)
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	generate := func(name string, flags map[string]string) (*CompactMetadata, CompactObjectVariant) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{
			Name:  name,
			Flags: flags,
		}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "arm64",
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		return metadata, variantByTarget(metadata, objectTarget(metadata, config, "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o"))
	}
	_, off := generate("off", nil)
	onMetadata, on := generate("on", map[string]string{"CONFIG_NVHE_LAYOUT": "y"})
	if off.Object == "" || on.Object == "" {
		t.Fatalf("missing nVHE variants: off=%#v on=%#v", off, on)
	}
	if got := on.configFragment["CONFIG_NVHE_LAYOUT"]; got != "y" {
		t.Fatalf("nVHE config fragment CONFIG_NVHE_LAYOUT = %q, want y", got)
	}
	if on.CompileEnvironment == off.CompileEnvironment || on.ContentID == off.ContentID {
		t.Fatalf("nVHE linker-script config did not split identity: off=%#v on=%#v", off, on)
	}
	onInputs, err := onMetadata.expandedSourceInputGroup(on.SourceInputGroup, "nVHE")
	if err != nil {
		t.Fatalf("expand nVHE source inputs: %v", err)
	}
	if !slices.ContainsFunc(onInputs, func(input CompactSourceInput) bool {
		return input.Path == "arch/arm64/kvm/hyp/nvhe/hyp.lds.S"
	}) {
		t.Fatalf("nVHE source inputs omit hyp.lds.S: %v", onInputs)
	}
	objectBuild, err := onMetadata.BuildFile(CompactBuildFileOptions{
		BaseConfig:         "on",
		Arch:               "arm64",
		SourceLabelPackage: "@linux//",
		SourceRootLabel:    "@linux//:Kconfig",
		Srcarch:            "arm64",
	})
	if err != nil {
		t.Fatalf("BuildFile() failed: %v", err)
	}
	parsed, err := build.ParseBuild("objects.BUILD.bazel", objectBuild)
	if err != nil {
		t.Fatalf("generated nVHE object BUILD did not parse: %v\n%s", err, objectBuild)
	}
	rule := parsed.RuleNamed(on.Target)
	if rule == nil {
		t.Fatalf("generated object BUILD has no nVHE rule %q:\n%s", on.Target, objectBuild)
	}
	if got := rule.AttrString("source_input_index"); got != ":_source_input_index" {
		t.Fatalf("nVHE source_input_index = %q", got)
	}
	if got := rule.AttrLiteral("source_input_group"); got != fmt.Sprintf("%d", on.SourceInputGroup) {
		t.Fatalf("nVHE source_input_group = %q, want %d", got, on.SourceInputGroup)
	}
	for _, attr := range []string{"source_includes", "source_includes_complete", "config_fragment"} {
		if rule.Attr(attr) != nil {
			t.Fatalf("indexed nVHE rule retains redundant %s:\n%s", attr, objectBuild)
		}
	}
	before := on.ContentID
	mustWriteSource(t, sourceRoot, "arch/arm64/kvm/hyp/nvhe/hyp.lds.S", `
#if defined(CONFIG_NVHE_LAYOUT)
SECTIONS { .text : { *(.text .text.*) } }
#endif
`)
	_, changed := generate("changed", map[string]string{"CONFIG_NVHE_LAYOUT": "y"})
	if changed.ContentID == before {
		t.Fatalf("nVHE linker-script digest did not change content ID %q", before)
	}
}

func TestCompactContentGraphArm64VDSO32WrapBindsNestedActionInputs(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"compat vDSO exact identity\"\n")
	kb, err := ParseKbuild(strings.NewReader(`
obj-y := arch/arm64/kernel/vdso32-wrap.o
`), "Makefile")
	if err != nil {
		t.Fatalf("parseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	for path, content := range map[string]string{
		"arch/arm64/include/asm/vdso/compat.h":       "#define COMPAT_VDSO 1\n",
		"arch/arm64/include/asm/vdso/gettimeofday.h": "#ifdef __aarch64__\n#include \"native.h\"\n#else\n#include \"compat.h\"\n#endif\n",
		"arch/arm64/include/asm/vdso/native.h":       "#define NATIVE_VDSO 1\n",
		"arch/arm64/kernel/vdso32-wrap.S":            ".incbin \"arch/arm64/kernel/vdso32/vdso.so\"\n",
		"arch/arm64/kernel/vdso32/note.c":            "int note;\n",
		"arch/arm64/kernel/vdso32/vdso.lds.S":        "SECTIONS { .text : { *(.text*) } }\n",
		"arch/arm64/kernel/vdso32/vgettimeofday.c":   "#include <asm/vdso/gettimeofday.h>\n",
		"lib/vdso/gettimeofday.c":                    "#include <asm/vdso/gettimeofday.h>\n",
	} {
		mustWriteSource(t, sourceRoot, path, content)
	}
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	generate := func(name string) (*CompactMetadata, CompactObjectVariant) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: name}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "arm64",
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		return metadata, variantByTarget(metadata, objectTarget(metadata, config, "arch/arm64/kernel/vdso32-wrap.o"))
	}

	metadata, before := generate("before")
	inputs, err := metadata.expandedSourceInputGroup(before.SourceInputGroup, "compat vDSO")
	if err != nil {
		t.Fatalf("expand compat vDSO source inputs: %v", err)
	}
	var paths []string
	for _, input := range inputs {
		paths = append(paths, input.Path)
	}
	for _, want := range []string{
		"arch/arm64/include/asm/vdso/compat.h",
		"arch/arm64/kernel/vdso32-wrap.S",
		"arch/arm64/kernel/vdso32/note.c",
		"arch/arm64/kernel/vdso32/vdso.lds.S",
		"arch/arm64/kernel/vdso32/vgettimeofday.c",
		"lib/vdso/gettimeofday.c",
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("compat vDSO source inputs = %v, want %q", paths, want)
		}
	}
	if slices.Contains(paths, "arch/arm64/include/asm/vdso/native.h") {
		t.Fatalf("compat vDSO source inputs selected native arm64 branch: %v", paths)
	}

	mustWriteSource(t, sourceRoot, "lib/vdso/gettimeofday.c", "#define COMPAT_CHANGED 1\n")
	_, changed := generate("changed")
	if changed.ContentID == before.ContentID {
		t.Fatalf("compat vDSO nested source digest did not change content ID %q", before.ContentID)
	}
}

func TestCompactContentGraphArm64VDSOWrapBindsGeneratedHeaderFamily(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"native vDSO exact identity\"\n")
	kb, err := ParseKbuild(strings.NewReader(`
obj-y := arch/arm64/kernel/vdso-wrap.o
`), "Makefile")
	if err != nil {
		t.Fatalf("parseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	for path, content := range map[string]string{
		"arch/arm64/include/asm/vdso/gettimeofday.h": "#include \"native.h\"\n",
		"arch/arm64/include/asm/vdso/native.h":       "#define NATIVE_VDSO 1\n",
		"arch/arm64/kernel/vdso-wrap.S":              ".incbin \"arch/arm64/kernel/vdso/vdso.so\"\n",
		"arch/arm64/kernel/vdso/note.c":              "int note;\n",
		"arch/arm64/kernel/vdso/vdso.lds.S":          "SECTIONS { .text : { *(.text*) } }\n",
		"arch/arm64/kernel/vdso/vgettimeofday.c":     "#include <asm/vdso/gettimeofday.h>\n",
		"lib/vdso/gettimeofday.c":                    "#include <asm/vdso/gettimeofday.h>\n",
	} {
		mustWriteSource(t, sourceRoot, path, content)
	}
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	generate := func(name string) (*CompactMetadata, CompactObjectVariant) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: name}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "arm64",
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		return metadata, variantByTarget(metadata, objectTarget(metadata, config, "arch/arm64/kernel/vdso-wrap.o"))
	}

	metadata, before := generate("before")
	if len(metadata.GeneratedHeaderFamilies) != 1 ||
		metadata.GeneratedHeaderFamilies[0].Name != compactGeneratedHeaderFamilyAll {
		t.Fatalf("native vDSO generated-header families = %#v, want one all family", metadata.GeneratedHeaderFamilies)
	}
	var environment CompactCompileEnvironment
	for _, candidate := range metadata.CompileEnvironments {
		if candidate.ID == before.CompileEnvironment {
			environment = candidate
			break
		}
	}
	wantFamilies := []string{metadata.GeneratedHeaderFamilies[0].ID}
	if !reflect.DeepEqual(environment.GeneratedHeaderFamilies, wantFamilies) {
		t.Fatalf("native vDSO generated-header families = %v, want %v", environment.GeneratedHeaderFamilies, wantFamilies)
	}

	mustWriteSource(t, sourceRoot, "lib/vdso/gettimeofday.c", "#define NATIVE_CHANGED 1\n")
	_, changed := generate("changed")
	if changed.ContentID == before.ContentID {
		t.Fatalf("native vDSO producer source digest did not change content ID %q", before.ContentID)
	}
}

func TestCompactContentGraphARMVDSOBindsExactGeneratedBinaryAndProducerInputs(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"ARM vDSO exact identity\"\n")
	kb, err := ParseKbuild(strings.NewReader("obj-y := arch/arm/vdso/vdso.o\n"), "Makefile")
	if err != nil {
		t.Fatalf("parseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	for path, content := range map[string]string{
		"arch/arm/vdso/vdso.S":          ".incbin \"arch/arm/vdso/vdso.so\"\n",
		"arch/arm/vdso/note.c":          "int note;\n",
		"arch/arm/vdso/vgettimeofday.c": "#ifdef BUILD_VDSO32\nint vdso_time;\n#endif\n",
		"arch/arm/vdso/vdso.lds.S":      "SECTIONS { .text : { *(.text*) } }\n",
		"arch/arm/vdso/vdsomunge.c":     "int host_vdsomunge;\n",
		"lib/vdso/gettimeofday.c":       "int generic_vdso_time;\n",
	} {
		mustWriteSource(t, sourceRoot, path, content)
	}
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	generate := func(name string) (*CompactMetadata, CompactObjectVariant) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: name}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "arm",
			CompileEnvironmentABI: "arm-object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		return metadata, variantByTarget(metadata, objectTarget(metadata, config, "arch/arm/vdso/vdso.o"))
	}

	metadata, before := generate("before")
	if len(metadata.GeneratedHeaderFamilies) != 1 ||
		metadata.GeneratedHeaderFamilies[0].Name != compactGeneratedHeaderFamilyAll {
		t.Fatalf("ARM vDSO generated-header families = %#v, want one all family", metadata.GeneratedHeaderFamilies)
	}
	family := metadata.GeneratedHeaderFamilies[0]
	inputs, err := metadata.expandedSourceInputGroup(family.SourceInputGroup, "ARM generated headers")
	if err != nil {
		t.Fatalf("expand ARM generated-header inputs: %v", err)
	}
	paths := sourceInputPaths(inputs)
	for _, want := range []string{
		"arch/arm/vdso/note.c",
		"arch/arm/vdso/vdso.lds.S",
		"arch/arm/vdso/vdsomunge.c",
		"arch/arm/vdso/vgettimeofday.c",
		"lib/vdso/gettimeofday.c",
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("ARM generated-header inputs = %v, want %q", paths, want)
		}
	}
	var environment CompactCompileEnvironment
	for _, candidate := range metadata.CompileEnvironments {
		if candidate.ID == before.CompileEnvironment {
			environment = candidate
			break
		}
	}
	if want := []string{family.ID}; !reflect.DeepEqual(environment.GeneratedHeaderFamilies, want) {
		t.Fatalf("ARM vDSO generated-header families = %v, want %v", environment.GeneratedHeaderFamilies, want)
	}

	mustWriteSource(t, sourceRoot, "lib/vdso/gettimeofday.c", "int generic_vdso_time_changed;\n")
	_, changed := generate("changed")
	if changed.ContentID == before.ContentID {
		t.Fatalf("ARM vDSO producer source digest did not change content ID %q", before.ContentID)
	}
}

func TestCompactContentGraphSpecialSourceManifestExcludesHostTools(t *testing.T) {
	inputs := compactSpecialSourcesForObject("arch/x86/entry/vdso/vdso-image-64.o")
	if inputs.primary != "arch/x86/entry/vdso/vdso-note.S" {
		t.Fatalf("vDSO primary source = %q", inputs.primary)
	}
	var paths []string
	for _, input := range inputs.inputs {
		paths = append(paths, input.path)
	}
	for _, want := range []string{
		"arch/x86/entry/vdso/vdso-note.S",
		"arch/x86/entry/vdso/vclock_gettime.c",
		"arch/x86/entry/vdso/vdso.lds.S",
		"arch/x86/include/asm/vdso.h",
	} {
		if !slices.Contains(paths, want) {
			t.Fatalf("vDSO source manifest %v missing %q", paths, want)
		}
	}
	if slices.Contains(paths, "arch/x86/entry/vdso/vdso2c.c") {
		t.Fatalf("vDSO source manifest includes host tool vdso2c.c: %v", paths)
	}
}

func TestCompactMappedGeneratedSourcesUseOutputLanguageFlags(t *testing.T) {
	for _, tc := range []struct {
		object    string
		source    string
		wantFlags []string
	}{
		{
			object:    "drivers/of/base.dtb.o",
			source:    "drivers/of/base.dts",
			wantFlags: []string{"-DANY", "-DASM_ONLY"},
		},
		{
			object:    "drivers/of/overlay.dtbo.o",
			source:    "drivers/of/overlay.dtso",
			wantFlags: []string{"-DANY", "-DASM_ONLY"},
		},
		{
			object:    "lib/crypto/arm/sha256-core.o",
			source:    "lib/crypto/arm/sha256-armv4.pl",
			wantFlags: []string{"-DANY", "-DASM_ONLY"},
		},
		{
			object:    "crypto/example.asn1.o",
			source:    "crypto/example.asn1",
			wantFlags: []string{"-DANY", "-DC_ONLY"},
		},
		{
			object:    "drivers/tty/vt/defkeymap.o",
			source:    "drivers/tty/vt/defkeymap.c_shipped",
			wantFlags: []string{"-DANY", "-DC_ONLY"},
		},
		{
			object:    "drivers/tty/vt/consolemap_deftbl.o",
			source:    "drivers/tty/vt/cp437.uni",
			wantFlags: []string{"-DANY", "-DC_ONLY"},
		},
		{
			object:    "arch/x86/kernel/cpu/capflags.o",
			source:    "arch/x86/kernel/cpu/mkcapflags.sh",
			wantFlags: []string{"-DANY", "-DC_ONLY"},
		},
	} {
		t.Run(filepath.Ext(tc.source), func(t *testing.T) {
			object := resolvedKbuildObject{
				object: tc.object,
				mode:   "y",
				flags: []resolvedKbuildFlag{
					{language: "any", values: []string{"-DANY"}},
					{language: "c", values: []string{"-DC_ONLY"}},
					{language: "asm", values: []string{"-DASM_ONLY"}},
				},
				footprint: map[string]bool{},
			}
			variant := object.variant(
				&ResolvedConfig{},
				tc.source,
				nil,
				nil,
				nil,
				nil,
				nil,
				"",
				nil,
				"linux.bzl/compact-v6/test",
				nil,
				"x86",
			)
			if got := variant.Flags; !reflect.DeepEqual(got, tc.wantFlags) {
				t.Fatalf("%s flags = %v, want %v", tc.source, got, tc.wantFlags)
			}
		})
	}
}

func TestNormalizeSourceRootFlagsWindowsPaths(t *testing.T) {
	const sourceRoot = `D:\_bazel\external\+linux_source_repository+linux_6_18_39`
	kb, err := parseKbuild(strings.NewReader(`
KBUILD_CFLAGS += -include $(srctree)/include/linux/hidden.h
obj-y += init.o
`), "Kbuild", map[string]string{
		"srctree": sourceRoot,
	}, ".")
	if err != nil {
		t.Fatalf("parseKbuild() failed: %v", err)
	}

	got := normalizeSourceRootFlags(kb.Flags[0].Flags, sourceRoot)
	want := []string{"-include", "$(srctree)/include/linux/hidden.h"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeSourceRootFlags() = %#v, want %#v", got, want)
	}

	got = normalizeSourceRootFlags([]string{
		`-includeD:\_bazel\external\+linux_source_repository+linux_6_18_39/include/linux/hidden.h`,
	}, sourceRoot)
	want = []string{`-include$(srctree)/include/linux/hidden.h`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeSourceRootFlags(joined) = %#v, want %#v", got, want)
	}

	variant := func(flags []string, root string) CompactObjectVariant {
		t.Helper()
		object := resolvedKbuildObject{
			object: "arch/x86/boot/startup/gdt_idt.pi.o",
			mode:   "y",
			flags: []resolvedKbuildFlag{{
				language: "c",
				values:   flags,
			}},
			footprint: map[string]bool{},
		}
		return object.variant(
			&ResolvedConfig{},
			"",
			nil,
			nil,
			nil,
			nil,
			nil,
			root,
			nil,
			"linux.bzl/compact-v6/test",
			nil,
			"x86",
		)
	}
	windowsVariant := variant(kb.Flags[0].Flags, sourceRoot)
	unixVariant := variant(
		[]string{"-include", "/workspace/linux/include/linux/hidden.h"},
		"/workspace/linux",
	)
	if windowsVariant.ContentID == "" ||
		windowsVariant.ContentID != unixVariant.ContentID ||
		windowsVariant.Target != unixVariant.Target {
		t.Fatalf(
			"host path split compact identity:\nwindows=%#v\nunix=%#v",
			windowsVariant,
			unixVariant,
		)
	}
}

func TestCompactBuildFileEmitsSourceLabels(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	sourceRoot := t.TempDir()
	writeCompactSource(t, sourceRoot, "subdir/init.c")
	writeCompactSource(t, sourceRoot, "subdir/net/core.c")

	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{
		{Name: "base", Flags: map[string]string{"CONFIG_NET": "y"}},
	}, CompactMetadataOptions{ObjectDir: "subdir", SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}

	config := configByName(metadata, "base")
	for object, wantSource := range map[string]string{
		"init.o":     "subdir/init.c",
		"net/core.o": "subdir/net/core.c",
	} {
		target := objectTarget(metadata, config, object)
		variant := variantByTarget(metadata, target)
		if variant.Source != wantSource {
			t.Fatalf("%s source = %q, want %q", object, variant.Source, wantSource)
		}
	}

	objectBuild, err := metadata.BuildFile(CompactBuildFileOptions{
		BaseConfig:         "base",
		Arch:               "x86",
		SourceLabelPackage: "@linux//",
		SourceObjtool:      "//linux:objtool",
		SourceRootLabel:    "@linux//:Kconfig",
	})
	if err != nil {
		t.Fatalf("BuildFile() failed: %v", err)
	}
	if _, err := build.ParseBuild("objects.BUILD.bazel", objectBuild); err != nil {
		t.Fatalf("generated object BUILD did not parse: %v\n%s", err, objectBuild)
	}
	for _, want := range []string{
		`linux_source_input_index(`,
		`source_tree_info = ":_source_tree"`,
		`objtool = "//linux:objtool"`,
		`"@linux//:subdir/init.c"`,
		`"@linux//:subdir/net/core.c"`,
		`source_input_index = ":_source_input_index"`,
	} {
		if !strings.Contains(string(objectBuild), want) {
			t.Fatalf("object BUILD missing %s:\n%s", want, objectBuild)
		}
	}
}

func TestCompactBuildFileLimitsSourceObjtoolToX86(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	sourceRoot := t.TempDir()
	writeCompactSource(t, sourceRoot, "subdir/init.c")
	writeCompactSource(t, sourceRoot, "subdir/net/core.c")

	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{
		{Name: "base", Flags: map[string]string{"CONFIG_NET": "y"}},
	}, CompactMetadataOptions{ObjectDir: "subdir", SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}
	objectBuild, err := metadata.BuildFile(CompactBuildFileOptions{
		BaseConfig:         "base",
		Arch:               "arm64",
		SourceLabelPackage: "@linux//",
		SourceObjtool:      "//linux:objtool",
		SourceRootLabel:    "@linux//:Kconfig",
	})
	if err != nil {
		t.Fatalf("BuildFile() failed: %v", err)
	}
	if strings.Contains(string(objectBuild), `objtool = "//linux:objtool"`) {
		t.Fatalf("arm64 object BUILD unexpectedly contains x86 objtool:\n%s", objectBuild)
	}
}

func TestCompactBuildFileEmitsSourceInputs(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	sourceRoot := t.TempDir()
	writeCompactSource(t, sourceRoot, "init.c")
	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{
		{Name: "base", Flags: nil},
	}, CompactMetadataOptions{SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}

	objectBuild, err := metadata.BuildFile(CompactBuildFileOptions{
		BaseConfig:         "base",
		SourceLabelPackage: "@linux//",
		SourceRootLabel:    "@linux//:Kconfig",
	})
	if err != nil {
		t.Fatalf("BuildFile() failed: %v", err)
	}
	for _, want := range []string{
		`linux_source_tree(`,
		`root = "@linux//:Kconfig"`,
		`source_tree_info = ":_source_tree"`,
	} {
		if !strings.Contains(string(objectBuild), want) {
			t.Fatalf("object BUILD missing %s:\n%s", want, objectBuild)
		}
	}
}

func TestCompactBuildFileEmitsQuotedIncludeClosure(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader("obj-y += init.o entry.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "init.c", `#include "fragments/first.inc"`+"\n")
	mustWriteSource(t, sourceRoot, "fragments/first.inc", `#include "../shared/second.inc"`+"\n")
	mustWriteSource(t, sourceRoot, "entry.S", `#include "asm/entry.inc"`+"\n")
	mustWriteSource(t, sourceRoot, "asm/entry.inc", `#include "../shared/second.inc"`+"\n")
	mustWriteSource(t, sourceRoot, "shared/second.inc", "int second;\n")
	mustWriteSource(t, sourceRoot, "shared/unrelated.inc", "int unrelated;\n")

	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{
		{Name: "base", Flags: nil},
	}, CompactMetadataOptions{SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}
	config := configByName(metadata, "base")
	for object, wantIncludes := range map[string][]string{
		"entry.o": {"asm/entry.inc", "shared/second.inc"},
		"init.o":  {"fragments/first.inc", "shared/second.inc"},
	} {
		variant := variantByTarget(metadata, objectTarget(metadata, config, object))
		inputs, err := metadata.expandedSourceInputGroup(variant.SourceInputGroup, object)
		if err != nil {
			t.Fatalf("expand %s inputs: %v", object, err)
		}
		for _, want := range wantIncludes {
			if !slices.ContainsFunc(inputs, func(input CompactSourceInput) bool {
				return input.Path == want
			}) {
				t.Fatalf("%s source inputs = %v, want %q", object, inputs, want)
			}
		}
	}
}

func TestCompactImageRulesAliasesDuplicateObjectSets(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{
		{Name: "base", Flags: nil},
		{Name: "copy", Flags: nil},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	imageBuild, err := metadata.imageBuildFile(CompactBuildFileOptions{BaseConfig: "base"})
	if err != nil {
		t.Fatalf("imageBuildFile() failed: %v", err)
	}
	if _, err := build.ParseBuild("images.BUILD.bazel", imageBuild); err != nil {
		t.Fatalf("image BUILD did not parse: %v\n%s", err, imageBuild)
	}
	if !strings.Contains(string(imageBuild), `alias(
    name = "copy_image",
    actual = ":base_image",
    tags = ["manual"],
)`) {
		t.Fatalf("image BUILD does not alias duplicate object set:\n%s", imageBuild)
	}
}

func TestCompactContentGraphImageBuildEmitsGroupedConfigProjections(t *testing.T) {
	id := func(value string) string {
		t.Helper()
		return strings.Repeat(value, 64)
	}
	metadata := &CompactMetadata{
		Configs: []CompactConfig{
			{
				Name:                "base",
				ObjectTargets:       []string{"a", "b"},
				ModuleObjectTargets: []string{"m", "n"},
				imageTarget:         "base_image",
			},
			{
				Name:                "copy",
				ObjectTargets:       []string{"a", "b"},
				ModuleObjectTargets: []string{"m", "n"},
				imageTarget:         "copy_image",
			},
			{
				Name:                "module_reorder",
				ObjectTargets:       []string{"a", "b"},
				ModuleObjectTargets: []string{"n", "m"},
				imageTarget:         "module_reorder_image",
			},
			{
				Name:                "overlay",
				ObjectTargets:       []string{"b", "c", "a"},
				ModuleObjectTargets: []string{"n"},
				imageTarget:         "overlay_image",
			},
		},
		ObjectVariants: []CompactObjectVariant{
			{Target: "a", Object: "a.o", ContentID: id("a")},
			{Target: "b", Object: "b.o", ContentID: id("b")},
			{Target: "c", Object: "c.o", ContentID: id("c")},
			{Target: "m", Object: "m.o", ContentID: id("d")},
			{Target: "n", Object: "n.o", ContentID: id("e")},
		},
	}
	imageBuild, err := metadata.imageBuildFile(CompactBuildFileOptions{
		Arch:               "x86",
		BaseConfig:         "base",
		ObjectLabelPackage: "//objects",
	})
	if err != nil {
		t.Fatalf("imageBuildFile() failed: %v", err)
	}
	parsed, err := build.ParseBuild("images.BUILD.bazel", imageBuild)
	if err != nil {
		t.Fatalf("generated image BUILD did not parse: %v\n%s", err, imageBuild)
	}
	base := parsed.RuleNamed("base_image")
	if base == nil || base.Kind() != "linux_grouped_compact_image" {
		t.Fatalf("base image is not a linux_grouped_compact_image:\n%s", imageBuild)
	}
	if got, want := base.AttrStrings("object_targets"), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("base object_targets = %v, want %v", got, want)
	}
	if got, want := base.AttrStrings("module_object_targets"), []string{"m", "n"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("base module_object_targets = %v, want %v", got, want)
	}
	copyRule := parsed.RuleNamed("copy_image")
	if copyRule == nil || copyRule.Kind() != "alias" || copyRule.AttrString("actual") != ":base_image" {
		t.Fatalf("identical config did not alias the base image:\n%s", imageBuild)
	}
	moduleReorder := parsed.RuleNamed("module_reorder_image")
	if moduleReorder == nil || moduleReorder.Kind() != "linux_grouped_compact_image" {
		t.Fatalf("module reorder with the same membership incorrectly aliased the base image:\n%s", imageBuild)
	}
	if got, want := moduleReorder.AttrStrings("module_object_targets"), []string{"n", "m"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("module reorder module_object_targets = %v, want %v", got, want)
	}
	overlay := parsed.RuleNamed("overlay_image")
	if overlay == nil || overlay.Kind() != "linux_grouped_compact_image" {
		t.Fatalf("overlay is not a linux_grouped_compact_image:\n%s", imageBuild)
	}
	if got, want := overlay.AttrStrings("object_targets"), []string{"b", "c", "a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay object_targets = %v, want %v", got, want)
	}
	if got, want := overlay.AttrStrings("module_object_targets"), []string{"n"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay module_object_targets = %v, want %v", got, want)
	}
}

func TestCompactMetadataPreservesFlagOrder(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader(`
obj-y += init.o
ccflags-y += -UCONFIG_NET -DCONFIG_NET=1
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{Name: "base", Flags: nil}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}
	target := objectTarget(metadata, configByName(metadata, "base"), "init.o")
	variant := variantByTarget(metadata, target)
	if got, want := strings.Join(variant.Flags, " "), "-UCONFIG_NET -DCONFIG_NET=1"; got != want {
		t.Fatalf("flags = %q, want %q", got, want)
	}
}

func TestCompactMetadataPreservesRemoveFlags(t *testing.T) {
	tree := mustParseCompactFixture(t)
	tmp := t.TempDir()
	kbuild := filepath.Join(tmp, "Kbuild")
	if err := os.WriteFile(kbuild, []byte(`obj-y += arch/arm64/lib/xor-neon.o
CFLAGS_arch/arm64/lib/xor-neon.o += $(CC_FLAGS_FPU)
CFLAGS_REMOVE_arch/arm64/lib/xor-neon.o += $(CC_FLAGS_NO_FPU)
`), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}
	kb, err := ParseKbuildFileWithOptions(kbuild, KbuildOptions{
		Variables: map[string]string{
			"CC_FLAGS_FPU":    "-ffreestanding -D_LINUX_FPU_COMPILATION_UNIT",
			"CC_FLAGS_NO_FPU": "-mgeneral-regs-only",
		},
	})
	if err != nil {
		t.Fatalf("ParseKbuildFileWithOptions() failed: %v", err)
	}
	metadata, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{{Name: "base", Flags: nil}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}
	target := objectTarget(metadata, configByName(metadata, "base"), "arch/arm64/lib/xor-neon.o")
	variant := variantByTarget(metadata, target)
	if got, want := strings.Join(variant.Flags, " "), "-ffreestanding -D_LINUX_FPU_COMPILATION_UNIT"; got != want {
		t.Fatalf("flags = %q, want %q", got, want)
	}
	if got, want := strings.Join(variant.RemoveFlags, " "), "-mgeneral-regs-only"; got != want {
		t.Fatalf("remove flags = %q, want %q", got, want)
	}
}

func TestCompactMetadataJSONEmitsEmptyTopLevelCollections(t *testing.T) {
	data, err := (&CompactMetadata{}).JSON()
	if err != nil {
		t.Fatalf("JSON() failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal JSON(): %v", err)
	}
	for _, field := range []string{
		"configs",
		"config_payloads",
		"compile_environments",
		"generated_header_families",
		"source_files",
		"source_input_groups",
		"object_variants",
	} {
		value, ok := decoded[field]
		if !ok {
			t.Errorf("JSON() omitted top-level collection %q:\n%s", field, data)
			continue
		}
		items, ok := value.([]any)
		if !ok || items == nil || len(items) != 0 {
			t.Errorf("JSON() field %q = %#v, want empty array", field, value)
		}
	}
}

func TestCompactMetadataRejectsDuplicateImageTargets(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	_, err := compactMetadataBatchForTest(t, tree, kb, []NamedConfig{
		{Name: "foo-bar", Flags: nil},
		{Name: "foo_bar", Flags: nil},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate image target") {
		t.Fatalf("CompactMetadata() error = %v, want duplicate image target error", err)
	}
}

func TestConfigRefsAcceptLowercaseSymbols(t *testing.T) {
	got := strings.Join(configRefs("-DCONFIG_foo=1 -DCONFIG_BAR"), ",")
	if want := "CONFIG_BAR,CONFIG_foo"; got != want {
		t.Fatalf("configRefs() = %q, want %q", got, want)
	}
}

func TestCompactMetadataNormalizesSourceRootFlags(t *testing.T) {
	tree := mustParseCompactFixture(t)
	sourceRoot := t.TempDir()
	writeCompactSource(t, sourceRoot, "lib/crypto/sha256.c")
	mustWriteSource(t, sourceRoot, "include/linux/hidden.h", "#define HIDDEN 1\n")
	kb, err := ParseKbuild(strings.NewReader(fmt.Sprintf(`
obj-y += lib/crypto/sha256.o
ccflags-y += -I%s/lib/crypto/x86 -include %s/include/linux/hidden.h
`, sourceRoot, sourceRoot)), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{
		{Name: "base", Flags: nil},
	}, CompactMetadataOptions{SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}

	config := configByName(metadata, "base")
	target := objectTarget(metadata, config, "lib/crypto/sha256.o")
	variant := variantByTarget(metadata, target)
	if got, want := strings.Join(variant.Flags, " "), "-I$(srctree)/lib/crypto/x86 -include $(srctree)/include/linux/hidden.h"; got != want {
		t.Fatalf("flags = %q, want %q", got, want)
	}
}

func TestCompactSourceBuildReadyRejectsUnknownMakeRefs(t *testing.T) {
	ready := CompactObjectVariant{
		Source:         "init.c",
		Flags:          []string{"-DNET=$(CONFIG_NET)", "-DPCI=${CONFIG_PCI}"},
		configFragment: map[string]string{"CONFIG_NET": "y", "CONFIG_PCI": "n"},
	}
	if !ready.sourceBuildReady() {
		t.Fatalf("sourceBuildReady() rejected config-only make refs")
	}

	unresolved := CompactObjectVariant{
		Source:         "version.c",
		Flags:          []string{"-include", "$(obj)/utsversion-tmp.h"},
		configFragment: map[string]string{},
	}
	if !unresolved.sourceBuildReady() {
		t.Fatalf("sourceBuildReady() rejected intrinsic obj make ref")
	}

	unknown := CompactObjectVariant{
		Source: "broken.c",
		Flags:  []string{"-I$(unsupported_dir)"},
	}
	if unknown.sourceBuildReady() {
		t.Fatalf("sourceBuildReady() accepted unknown make ref")
	}
	if got := unknown.sourceBuildError(); !strings.Contains(got, "unsupported_dir") {
		t.Fatalf("sourceBuildError() = %q, want unsupported variable context", got)
	}
}

func TestSourceCandidatesForGeneratedArchitectureObjects(t *testing.T) {
	tests := map[string][]string{
		"arch/arm64/kernel/pi/lib-fdt.pi.o": {
			"lib/fdt.c",
			"arch/arm64/kernel/pi/fdt.c",
		},
		"arch/riscv/kernel/pi/ctype.pi.o": {
			"lib/ctype.c",
		},
		"arch/riscv/kernel/pi/string.pi.o": {
			"lib/string.c",
		},
		"arch/riscv/kernel/pi/lib-fdt.pi.o": {
			"lib/fdt.c",
		},
		"drivers/of/empty_root.dtb.o": {
			"drivers/of/empty_root.dts",
		},
		"arch/arm64/kvm/hyp/nvhe/switch.nvhe.o": {
			"arch/arm64/kvm/hyp/nvhe/switch.c",
		},
		"lib/crypto/arm64/poly1305-core.o": {
			"lib/crypto/arm64/poly1305-armv8.pl",
		},
		"lib/crypto/arm64/sha256-core.o": {
			"lib/crypto/arm64/sha2-armv8.pl",
		},
		"lib/crypto/arm64/sha512-core.o": {
			"lib/crypto/arm64/sha2-armv8.pl",
		},
		"lib/crypto/arm/poly1305-core.o": {
			"lib/crypto/arm/poly1305-armv4.pl",
		},
		"lib/crypto/arm/sha256-core.o": {
			"lib/crypto/arm/sha256-armv4.pl",
		},
		"lib/crypto/arm/sha512-core.o": {
			"lib/crypto/arm/sha512-armv4.pl",
		},
		"lib/crypto/riscv/poly1305-core.o": {
			"lib/crypto/riscv/poly1305-riscv.pl",
		},
	}
	for object, wantCandidates := range tests {
		got := sourceCandidatesForObject(object)
		for _, want := range wantCandidates {
			if !slices.Contains(got, want) {
				t.Fatalf("sourceCandidatesForObject(%q) = %v, want candidate %q", object, got, want)
			}
		}
	}
}

func TestCompactContentGraphEFILibstubLibFDTUsesVendoredIncludeRoot(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"EFI libstub libfdt closure\"\n")
	kb, err := ParseKbuild(strings.NewReader(
		"obj-y := drivers/firmware/efi/libstub/lib-fdt.stub.o\n",
	), "Makefile")
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	for path, content := range map[string]string{
		"lib/fdt.c":                            "#include <linux/libfdt_env.h>\n#include \"../scripts/dtc/libfdt/fdt.c\"\n",
		"include/linux/libfdt_env.h":           "#define LIBFDT_ENV_H 1\n",
		"scripts/dtc/libfdt/fdt.c":             "#include \"libfdt_env.h\"\n#include <libfdt.h>\n#include \"libfdt_internal.h\"\n",
		"scripts/dtc/libfdt/libfdt_env.h":      "#ifndef LIBFDT_ENV_H\n#include <stddef.h>\n#endif\n",
		"scripts/dtc/libfdt/libfdt.h":          "#include <fdt.h>\n",
		"scripts/dtc/libfdt/fdt.h":             "#define FDT_MAGIC 1\n",
		"scripts/dtc/libfdt/libfdt_internal.h": "#define FDT_INTERNAL 1\n",
	} {
		mustWriteSource(t, sourceRoot, path, content)
	}
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	generate := func(name string) (*CompactMetadata, CompactObjectVariant) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: name}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "riscv",
			CompileEnvironmentABI: "riscv-efi-stub-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		return metadata, variantByTarget(metadata, objectTarget(metadata, config, "drivers/firmware/efi/libstub/lib-fdt.stub.o"))
	}

	metadata, before := generate("before")
	if before.Source != "lib/fdt.c" {
		t.Fatalf("EFI libstub lib-fdt source = %q, want lib/fdt.c", before.Source)
	}
	inputs, err := metadata.expandedSourceInputGroup(before.SourceInputGroup, "EFI libstub libfdt")
	if err != nil {
		t.Fatal(err)
	}
	paths := sourceInputPaths(inputs)
	for _, want := range []string{
		"lib/fdt.c",
		"include/linux/libfdt_env.h",
		"scripts/dtc/libfdt/fdt.c",
		"scripts/dtc/libfdt/libfdt_env.h",
		"scripts/dtc/libfdt/libfdt.h",
		"scripts/dtc/libfdt/fdt.h",
		"scripts/dtc/libfdt/libfdt_internal.h",
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("EFI libstub libfdt inputs = %v, want %q", paths, want)
		}
	}

	mustWriteSource(t, sourceRoot, "scripts/dtc/libfdt/fdt.h", "#define FDT_MAGIC 2\n")
	_, changed := generate("changed")
	if changed.ContentID == before.ContentID {
		t.Fatalf("vendored fdt.h did not change EFI stub content ID %q", before.ContentID)
	}
}

func TestCompactContentGraphRISCVPIObjectsBindPreparedSources(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"RISC-V PI object preparation\"\n")
	sourceRoot := t.TempDir()
	for path, content := range map[string]string{
		"Kbuild": "obj-y += arch/riscv/kernel/pi/\n",
		"arch/riscv/kernel/pi/Makefile": `KBUILD_CFLAGS := -fpie -Os -I$(srctree)/scripts/dtc/libfdt
CFLAGS_ctype.o += -D__NO_FORTIFY
obj-y := ctype.pi.o string.pi.o lib-fdt.pi.o
`,
		"lib/ctype.c":  "int kernel_ctype_v1;\n",
		"lib/string.c": "int kernel_string;\n",
		"lib/fdt.c":    "int kernel_fdt;\n",
	} {
		mustWriteSource(t, sourceRoot, path, content)
	}
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	kb, err := ParseKbuildDirectoryTree(filepath.Join(sourceRoot, "Kbuild"), KbuildOptions{
		RootDir:   sourceRoot,
		Variables: map[string]string{"SRCARCH": "riscv"},
	})
	if err != nil {
		t.Fatalf("ParseKbuildDirectoryTree() failed: %v", err)
	}
	generate := func(name string) (*CompactMetadata, map[string]CompactObjectVariant) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: name}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "riscv",
			CompileEnvironmentABI: "riscv-pi-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		variants := map[string]CompactObjectVariant{}
		for _, object := range []string{
			"arch/riscv/kernel/pi/ctype.pi.o",
			"arch/riscv/kernel/pi/string.pi.o",
			"arch/riscv/kernel/pi/lib-fdt.pi.o",
		} {
			variants[object] = variantByTarget(metadata, objectTarget(metadata, config, object))
		}
		return metadata, variants
	}

	metadata, before := generate("before")
	wantSources := map[string]string{
		"arch/riscv/kernel/pi/ctype.pi.o":   "lib/ctype.c",
		"arch/riscv/kernel/pi/string.pi.o":  "lib/string.c",
		"arch/riscv/kernel/pi/lib-fdt.pi.o": "lib/fdt.c",
	}
	for object, wantSource := range wantSources {
		variant := before[object]
		if variant.Source != wantSource {
			t.Errorf("%s source = %q, want %q", object, variant.Source, wantSource)
		}
		inputs, err := metadata.expandedSourceInputGroup(variant.SourceInputGroup, object)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(sourceInputPaths(inputs), wantSource) {
			t.Errorf("%s inputs = %v, want %q", object, sourceInputPaths(inputs), wantSource)
		}
		if !slices.Contains(variant.Flags, "-fpie") {
			t.Errorf("%s flags = %v, want PI compilation", object, variant.Flags)
		}
	}
	ctypeObject := "arch/riscv/kernel/pi/ctype.pi.o"
	if !slices.Contains(before[ctypeObject].Flags, "-D__NO_FORTIFY") {
		t.Fatalf("ctype PI flags = %v, want compile-intermediate CFLAGS_ctype.o", before[ctypeObject].Flags)
	}
	if slices.Contains(before["arch/riscv/kernel/pi/string.pi.o"].Flags, "-D__NO_FORTIFY") {
		t.Fatalf("string PI inherited ctype-only flags: %v", before["arch/riscv/kernel/pi/string.pi.o"].Flags)
	}

	mustWriteSource(t, sourceRoot, "lib/ctype.c", "int kernel_ctype_v2;\n")
	_, changed := generate("changed")
	if changed[ctypeObject].ContentID == before[ctypeObject].ContentID {
		t.Fatalf("RISC-V ctype PI source digest did not change content ID %q", before[ctypeObject].ContentID)
	}
	stringObject := "arch/riscv/kernel/pi/string.pi.o"
	if changed[stringObject].ContentID != before[stringObject].ContentID {
		t.Fatalf("ctype source change perturbed unrelated string PI object")
	}
}

func TestCompactContentGraphGeneratedARMPerlasmSourceIdentity(t *testing.T) {
	tree := mustParseString(t, "mainmenu \"ARM perlasm identity\"\n")
	kb, err := ParseKbuild(strings.NewReader("obj-y := lib/crypto/arm/sha256-core.o\n"), "Makefile")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	const generator = "lib/crypto/arm/sha256-armv4.pl"
	mustWriteSource(t, sourceRoot, generator, "# ARM SHA-256 generator v1\n")
	writeCompactContentGraphForcedInputs(t, sourceRoot)
	generate := func() (*CompactMetadata, CompactObjectVariant) {
		t.Helper()
		metadata, err := compactMetadataBatchWithOptionsForTest(t, tree, kb, []NamedConfig{{Name: "arm"}}, CompactMetadataOptions{
			SourceRoot:            sourceRoot,
			Srcarch:               "arm",
			CompileEnvironmentABI: "arm-object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
		}
		config := configByName(metadata, "arm")
		return metadata, variantByTarget(metadata, objectTarget(metadata, config, "lib/crypto/arm/sha256-core.o"))
	}

	metadata, before := generate()
	if before.Source != generator {
		t.Fatalf("ARM SHA-256 source = %q, want %q", before.Source, generator)
	}
	inputs, err := metadata.expandedSourceInputGroup(before.SourceInputGroup, "ARM SHA-256")
	if err != nil {
		t.Fatalf("expand ARM SHA-256 source inputs: %v", err)
	}
	if got := sourceInputByPath(inputs, generator).Path; got == "" {
		t.Fatalf("ARM SHA-256 source inputs = %v, want %q", inputs, generator)
	}
	mustWriteSource(t, sourceRoot, generator, "# ARM SHA-256 generator v2\n")
	_, changed := generate()
	if changed.ContentID == before.ContentID {
		t.Fatalf("ARM SHA-256 generator change did not change content ID %q", before.ContentID)
	}
}

func mustParseCompactFixture(t *testing.T) *Tree {
	t.Helper()
	tree, err := Parse(context.Background(), strings.NewReader(compactKconfigFixture), "Kconfig", Options{})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	return tree
}

func mustParseKbuildFixture(t *testing.T) *KbuildFile {
	t.Helper()
	kb, err := ParseKbuild(strings.NewReader(compactKbuildFixture), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	return kb
}

func writeCompactSource(t *testing.T, root, rel string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) failed: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("int source;\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) failed: %v", path, err)
	}
}

func writeCompactContentGraphForcedInputs(t *testing.T, root string) {
	t.Helper()
	for _, path := range []string{
		"include/linux/compiler-version.h",
		"include/linux/compiler_types.h",
		"include/linux/kconfig.h",
	} {
		mustWriteSource(t, root, path, "\n")
	}
}

func compactMetadataBatchForTest(
	t *testing.T,
	tree *Tree,
	kb *KbuildFile,
	configs []NamedConfig,
) (*CompactMetadata, error) {
	t.Helper()
	return compactMetadataBatchWithOptionsForTest(t, tree, kb, configs, CompactMetadataOptions{})
}

func compactMetadataBatchWithOptionsForTest(
	t *testing.T,
	tree *Tree,
	kb *KbuildFile,
	configs []NamedConfig,
	opts CompactMetadataOptions,
) (*CompactMetadata, error) {
	t.Helper()
	if opts.CompileEnvironmentABI == "" {
		opts.CompileEnvironmentABI = "linux.bzl/test"
	}
	if opts.Srcarch == "" {
		opts.Srcarch = "x86"
	}
	if opts.SourceRoot == "" && len(opts.SourceRoots) == 0 {
		opts.SourceRoot = t.TempDir()
	}
	if opts.SourceRoot != "" {
		if opts.Srcarch == "arm64" {
			for _, dir := range []string{
				"arch/arm64/kernel/vdso",
				"arch/arm64/kernel/vdso32",
			} {
				if err := os.MkdirAll(filepath.Join(opts.SourceRoot, filepath.FromSlash(dir)), 0o755); err != nil {
					return nil, err
				}
			}
		}
		for _, named := range configs {
			resolved, err := tree.ResolveConfigWithOptions(named.Name, named.Flags, ResolveConfigOptions{
				AllNoConfig: named.AllNoConfig,
			})
			if err != nil {
				return nil, err
			}
			for _, object := range kb.resolvedObjects(resolved).all() {
				if len(object.members) != 0 ||
					sourceForObject(opts.SourceRoot, opts.ObjectDir, object.object, opts.SourceRoots) != "" {
					continue
				}
				candidates := sourceCandidatesForObject(object.object)
				source := strings.TrimSuffix(object.object, ".o") + ".c"
				for _, flag := range object.flags {
					if flag.language == "asm" {
						source = strings.TrimSuffix(object.object, ".o") + ".S"
						break
					}
				}
				if len(candidates) != 0 {
					source = candidates[0]
				}
				writeCompactSource(t, opts.SourceRoot, filepath.ToSlash(filepath.Join(opts.ObjectDir, source)))
			}
		}
		for _, path := range []string{
			"include/linux/compiler-version.h",
			"include/linux/compiler_types.h",
			"include/linux/kconfig.h",
		} {
			if !fileExists(filepath.Join(opts.SourceRoot, filepath.FromSlash(path))) {
				mustWriteSource(t, opts.SourceRoot, path, "\n")
			}
		}
	}
	return tree.CompactMetadataBatchWithOptions(configs, opts, func(config *ResolvedConfig) (CompactConfigGraph, error) {
		return CompactConfigGraph{
			Kbuild:                kb,
			GeneratedHeadersLabel: "//internal/kconfig:test_generated_headers_" + sanitizeTargetName(config.Name),
		}, nil
	})
}

func configByName(metadata *CompactMetadata, name string) *CompactConfig {
	for i := range metadata.Configs {
		if metadata.Configs[i].Name == name {
			return &metadata.Configs[i]
		}
	}
	return nil
}

func variantByTarget(metadata *CompactMetadata, target string) CompactObjectVariant {
	for _, variant := range metadata.ObjectVariants {
		if variant.Target == target {
			return variant
		}
	}
	return CompactObjectVariant{}
}

func sourceInputByPath(inputs []CompactSourceInput, path string) CompactSourceInput {
	for _, input := range inputs {
		if input.Path == path {
			return input
		}
	}
	return CompactSourceInput{}
}

func objectTarget(metadata *CompactMetadata, config *CompactConfig, object string) string {
	if config == nil {
		return ""
	}
	for _, target := range config.ObjectTargets {
		if variantByTarget(metadata, target).Object == object {
			return target
		}
	}
	return ""
}

func objectNames(metadata *CompactMetadata, targets []string) []string {
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		out = append(out, variantByTarget(metadata, target).Object)
	}
	return out
}

func moduleObjectTarget(metadata *CompactMetadata, config *CompactConfig, object string) string {
	if config == nil {
		return ""
	}
	for _, target := range config.ModuleObjectTargets {
		if variantByTarget(metadata, target).Object == object {
			return target
		}
	}
	return ""
}

func compositeMemberTarget(metadata *CompactMetadata, config *CompactConfig, composite, member string) string {
	parent := variantByTarget(metadata, objectTarget(metadata, config, composite))
	for _, target := range parent.Members {
		if variantByTarget(metadata, target).Object == member {
			return target
		}
	}
	return ""
}
