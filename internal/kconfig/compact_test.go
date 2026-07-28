package kconfig

import (
	"context"
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
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{
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

func TestCompactMetadataUsesEffectiveSelectedValues(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{
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
	if got := variant.ConfigFragment["CONFIG_NET"]; got != "y" {
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
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := tree.CompactMetadata(kb, []NamedConfig{
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
	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{
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
	}}, CompactMetadataOptions{Schema: CompactSchemaV012})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
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
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{
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

	metadata, err := tree.CompactMetadata(kb, []NamedConfig{
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

	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base"}})
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

	metadata, err := tree.CompactMetadata(kb, []NamedConfig{
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

	metadata, err := tree.CompactMetadata(kb, []NamedConfig{
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

	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base"}})
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
	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
		Schema:     CompactSchemaV012,
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

	objectBuild, err := metadata.ObjectBuildFile(CompactBuildFileOptions{
		Schema:             CompactSchemaV012,
		Arch:               "x86",
		SourceLabelPackage: "@linux//",
		SourceObjtool:      "//linux:objtool",
		SourceRootLabel:    "@linux//:Kconfig",
	})
	if err != nil {
		t.Fatalf("ObjectBuildFile() failed: %v", err)
	}
	parsed, err := build.ParseBuild("objects.BUILD.bazel", objectBuild)
	if err != nil {
		t.Fatalf("generated object BUILD did not parse: %v\n%s", err, objectBuild)
	}
	if got := parsed.RuleNamed(head.Target).AttrString("objtool"); got != "" {
		t.Fatalf("head.o objtool = %q, want omitted", got)
	}
	if got := parsed.RuleNamed(normal.Target).AttrString("objtool"); got != "//linux:objtool" {
		t.Fatalf("normal.o objtool = %q, want //linux:objtool", got)
	}
	if got := parsed.RuleNamed(module.Target).AttrString("objtool"); got != "" {
		t.Fatalf("module.o objtool = %q, want module-root processing only", got)
	}
	if !module.ModuleRoot || parsed.RuleNamed(module.Target).AttrLiteral("module_root") != "True" {
		t.Fatalf("module.o did not preserve its single-module root marker")
	}
	if got := parsed.RuleNamed(startup.Target).AttrStrings("objtool_args"); !reflect.DeepEqual(got, []string{"--noabs"}) {
		t.Fatalf("startup.pi.o objtool_args = %q, want [--noabs]", got)
	}
	if !strings.Contains(string(objectBuild), "objtool_force = True") {
		t.Fatalf("startup.pi.o BUILD rule is missing objtool_force:\n%s", objectBuild)
	}
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
	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
		Schema:     CompactSchemaV012,
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

	objectBuild, err := metadata.ObjectBuildFile(CompactBuildFileOptions{
		Schema:             CompactSchemaV012,
		Arch:               "x86",
		SourceLabelPackage: "@linux//",
		SourceObjtool:      "//linux:objtool",
		SourceRootLabel:    "@linux//:Kconfig",
	})
	if err != nil {
		t.Fatalf("ObjectBuildFile() failed: %v", err)
	}
	parsed, err := build.ParseBuild("objects.BUILD.bazel", objectBuild)
	if err != nil {
		t.Fatalf("generated object BUILD did not parse: %v\n%s", err, objectBuild)
	}
	singleRule := parsed.RuleNamed(single.Target)
	if singleRule.Kind() != "linux_object" ||
		singleRule.AttrLiteral("module_root") != "True" ||
		singleRule.AttrString("objtool") != "//linux:objtool" {
		t.Fatalf("single.o rule does not carry single-module objtool metadata:\n%s", objectBuild)
	}
	multiRule := parsed.RuleNamed(multi.Target)
	if multiRule.Kind() != "linux_composite_object" || multiRule.AttrLiteral("module_root") != "True" {
		t.Fatalf("multi.o rule does not carry composite-module metadata:\n%s", objectBuild)
	}
	if got := parsed.RuleNamed(member.Target).AttrString("objtool"); got != "//linux:objtool" {
		t.Fatalf("member.o objtool = %q, want //linux:objtool", got)
	}
	if got := parsed.RuleNamed(skipped.Target).AttrString("objtool"); got != "" {
		t.Fatalf("skipped.o objtool = %q, want omitted", got)
	}
	forcedRule := parsed.RuleNamed(forced.Target)
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

	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base"}})
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
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base"}})
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
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base"}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	base := configByName(metadata, "base")
	if got, want := objectNames(metadata, base.ObjectTargets), []string{"early.o", "child/child.o", "late.o", "head.o"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ObjectTargets order = %#v, want %#v", got, want)
	}
}

func TestCompactMetadataSplicesExtraKbuildIntoParentDirectory(t *testing.T) {
	tree := mustParseCompactFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(`obj-y += fs/
obj-y += security/
`), 0o644); err != nil {
		t.Fatalf("WriteFile(Kbuild) failed: %v", err)
	}
	for subdir, content := range map[string]string{
		"fs":       "obj-y += base.o\n",
		"security": "obj-y += commoncap.o\n",
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
	extra, err := ParseKbuild(strings.NewReader(`obj-y += actiondfs.o
`), "Makefile")
	if err != nil {
		t.Fatalf("ParseKbuild(extra) failed: %v", err)
	}
	kb = MergeKbuildFileAtDirectory(kb, "fs/actiondfs", PrefixKbuildFile(extra, "fs/actiondfs"))
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base"}})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	base := configByName(metadata, "base")
	if got, want := objectNames(metadata, base.ObjectTargets), []string{"fs/base.o", "fs/actiondfs/actiondfs.o", "security/commoncap.o"}; strings.Join(got, ",") != strings.Join(want, ",") {
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
	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
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
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base"}})
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
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base", Flags: nil}})
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
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base", Flags: nil}})
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
	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{
		Name:  "base",
		Flags: map[string]string{"CONFIG_RUST": "y"},
	}}, CompactMetadataOptions{Schema: CompactSchemaV012})
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
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base", Flags: map[string]string{"CONFIG_EFI_STUB": "y"}}})
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
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base", Flags: map[string]string{"CONFIG_EFI_STUB": "y"}}})
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

func TestCompactMetadataGroupsObjectVariantsByPackage(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{
		{Name: "debug", Flags: map[string]string{"CONFIG_DEBUG": "y", "CONFIG_NET": "y"}},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	seen := map[string]bool{}
	for _, pkg := range metadata.ObjectPackages {
		if len(pkg.ObjectTargets) == 0 {
			t.Fatalf("package %q has no object targets", pkg.Package)
		}
		seen[pkg.Package] = true
		for _, target := range pkg.ObjectTargets {
			variant := variantByTarget(metadata, target)
			if variant.Target == "" {
				t.Fatalf("package %q references missing target %q", pkg.Package, target)
			}
			if variant.Package != pkg.Package {
				t.Fatalf("target %q package = %q, want %q", target, variant.Package, pkg.Package)
			}
		}
	}
	for _, want := range []string{"", "net"} {
		if !seen[want] {
			t.Fatalf("missing object package %q in %#v", want, metadata.ObjectPackages)
		}
	}
}

func TestCompactBuildFilesParse(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	sourceRoot := t.TempDir()
	writeCompactSource(t, sourceRoot, "init.c")
	writeCompactSource(t, sourceRoot, "net/core.c")
	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "base", Flags: map[string]string{"CONFIG_NET": "y"}},
	}, CompactMetadataOptions{Schema: CompactSchemaV012, SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}

	objectBuild, err := metadata.ObjectBuildFile(CompactBuildFileOptions{
		Schema:             CompactSchemaV012,
		RuleLoadLabel:      "//rules:linux_objects.bzl",
		SourceLabelPackage: "//linux",
		SourceRootLabel:    "//linux:Kconfig",
	})
	if err != nil {
		t.Fatalf("ObjectBuildFile() failed: %v", err)
	}
	if _, err := build.ParseBuild("objects.BUILD.bazel", objectBuild); err != nil {
		t.Fatalf("object BUILD did not parse: %v\n%s", err, objectBuild)
	}

	if !strings.Contains(string(objectBuild), `load("//rules:linux_objects.bzl", "linux_config", "linux_object", "linux_source_tree")`) {
		t.Fatalf("object BUILD does not use custom compact rule load label:\n%s", objectBuild)
	}

	imageBuild, err := metadata.ImageBuildFile(CompactImageBuildFileOptions{
		Schema:             CompactSchemaV012,
		ObjectLabelPackage: "linux/objects",
		RuleLoadLabel:      "//rules:linux_objects.bzl",
	})
	if err != nil {
		t.Fatalf("ImageBuildFile() failed: %v", err)
	}
	if _, err := build.ParseBuild("images.BUILD.bazel", imageBuild); err != nil {
		t.Fatalf("image BUILD did not parse: %v\n%s", err, imageBuild)
	}
	if !strings.Contains(string(imageBuild), `"//linux/objects:`) {
		t.Fatalf("image BUILD does not reference object package:\n%s", imageBuild)
	}
	if !strings.Contains(string(imageBuild), `load("//rules:linux_objects.bzl", "linux_compact_image")`) {
		t.Fatalf("image BUILD does not use custom compact rule load label:\n%s", imageBuild)
	}
	if strings.Contains(string(imageBuild), "require_real") {
		t.Fatalf("image BUILD contains removed require_real compatibility attribute:\n%s", imageBuild)
	}
}

func TestCompactSchemasKeepLegacyImageContractOptIn(t *testing.T) {
	metadata := &CompactMetadata{
		Configs: []CompactConfig{{
			Name:                "base",
			ImageTarget:         "base_image",
			ObjectTargets:       []string{"builtin"},
			ModuleObjectTargets: []string{"module"},
		}},
	}
	legacy, err := metadata.ImageBuildFile(CompactImageBuildFileOptions{
		RequireReal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacy), "require_real = True") {
		t.Fatalf("default schema lost legacy require_real:\n%s", legacy)
	}
	if strings.Contains(string(legacy), "module_objects") {
		t.Fatalf("default schema emitted v0.0.12 module_objects:\n%s", legacy)
	}

	native, err := metadata.ImageBuildFile(CompactImageBuildFileOptions{
		Schema:      CompactSchemaV012,
		RequireReal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(native), "require_real") {
		t.Fatalf("v0.0.12 schema emitted legacy require_real:\n%s", native)
	}
	if !strings.Contains(string(native), `module_objects = ["//:module"]`) {
		t.Fatalf("v0.0.12 schema omitted module_objects:\n%s", native)
	}
}

func TestParseCompactSchema(t *testing.T) {
	for _, value := range []string{string(CompactSchemaV011), string(CompactSchemaV012)} {
		if got, err := ParseCompactSchema(value); err != nil || string(got) != value {
			t.Fatalf("ParseCompactSchema(%q) = %q, %v", value, got, err)
		}
	}
	if _, err := ParseCompactSchema("next"); err == nil {
		t.Fatal("ParseCompactSchema() accepted an unknown schema")
	}
}

func TestCompactMetadataJSONMatchesPrettierArrayLayout(t *testing.T) {
	metadata := &CompactMetadata{
		Configs: []CompactConfig{
			{
				Name:        "base",
				ImageTarget: "base_image",
				ObjectTargets: []string{
					"first_extremely_long_object_target_name_for_layout_testing",
					"second_extremely_long_object_target_name_for_layout_testing",
				},
			},
		},
		ObjectVariants: []CompactObjectVariant{
			{
				Target: "base__short",
				Object: "base.o",
				Source: "base.c",
				Mode:   "y",
				Flags:  []string{"-I", "external/+linux_kernel+linux_6_18_2/arch/x86/kvm"},
			},
		},
	}

	data, err := metadata.JSON()
	if err != nil {
		t.Fatalf("JSON() failed: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `      "flags": ["-I", "external/+linux_kernel+linux_6_18_2/arch/x86/kvm"]`) {
		t.Fatalf("JSON() did not compact short string array:\n%s", text)
	}
	if !strings.Contains(text, "      \"object_targets\": [\n        \"first_extremely_long_object_target_name_for_layout_testing\",") {
		t.Fatalf("JSON() compacted long string array:\n%s", text)
	}
}

func TestNormalizeSourceRootFlagsWindowsPaths(t *testing.T) {
	root := `C:\users\runneradmin\execroot\external\linux`
	got := normalizeSourceRootFlags([]string{
		`-includeC:\users\runneradmin\execroot\external\linux\include\linux\hidden.h`,
		`-imacrosC:\users\runneradmin\execroot\external\linux\include\linux\macros.h`,
		`-idirafterC:\users\runneradmin\execroot\external\linux\private`,
	}, root)
	want := []string{
		`-include$(srctree)/include/linux/hidden.h`,
		`-imacros$(srctree)/include/linux/macros.h`,
		`-idirafter$(srctree)/private`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeSourceRootFlags() = %#v, want %#v", got, want)
	}
}

func TestCompactObjectBuildFileEmitsSourceLabels(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	sourceRoot := t.TempDir()
	writeCompactSource(t, sourceRoot, "subdir/init.c")
	writeCompactSource(t, sourceRoot, "subdir/net/core.c")

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "base", Flags: map[string]string{"CONFIG_NET": "y"}},
	}, CompactMetadataOptions{Schema: CompactSchemaV012, ObjectDir: "subdir", SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}

	config := configByName(metadata, "base")
	initTarget := objectTarget(metadata, config, "init.o")
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

	objectBuild, err := metadata.ObjectBuildFile(CompactBuildFileOptions{
		Schema:             CompactSchemaV012,
		Arch:               "x86",
		SourceLabelPackage: "@linux//",
		SourceObjtool:      "//linux:objtool",
		SourceRootLabel:    "@linux//:Kconfig",
	})
	if err != nil {
		t.Fatalf("ObjectBuildFile() failed: %v", err)
	}
	parsed, err := build.ParseBuild("objects.BUILD.bazel", objectBuild)
	if err != nil {
		t.Fatalf("generated object BUILD did not parse: %v\n%s", err, objectBuild)
	}
	configRule := parsed.RuleNamed(initTarget + "_config")
	if configRule == nil || configRule.Kind() != "linux_config" {
		t.Fatalf("generated object BUILD missing linux_config %q:\n%s", initTarget+"_config", objectBuild)
	}
	if got := configRule.AttrString("arch"); got != "x86" {
		t.Fatalf("generated linux_config arch = %q, want x86:\n%s", got, objectBuild)
	}
	for _, want := range []string{
		`load("@linux.bzl//internal:linux_objects.bzl", "linux_config", "linux_object", "linux_source_tree")`,
		`linux_config(`,
		`source_tree_info = ":_source_tree"`,
		`name = "` + initTarget + `_config"`,
		`config = ":` + initTarget + `_config"`,
		`objtool = "//linux:objtool"`,
		`src = "@linux//:subdir/init.c"`,
		`src = "@linux//:subdir/net/core.c"`,
	} {
		if !strings.Contains(string(objectBuild), want) {
			t.Fatalf("object BUILD missing %s:\n%s", want, objectBuild)
		}
	}
}

func TestCompactObjectBuildFileLimitsSourceObjtoolToX86(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	sourceRoot := t.TempDir()
	writeCompactSource(t, sourceRoot, "subdir/init.c")
	writeCompactSource(t, sourceRoot, "subdir/net/core.c")

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "base", Flags: map[string]string{"CONFIG_NET": "y"}},
	}, CompactMetadataOptions{Schema: CompactSchemaV012, ObjectDir: "subdir", SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}
	objectBuild, err := metadata.ObjectBuildFile(CompactBuildFileOptions{
		Schema:             CompactSchemaV012,
		Arch:               "arm64",
		SourceLabelPackage: "@linux//",
		SourceObjtool:      "//linux:objtool",
		SourceRootLabel:    "@linux//:Kconfig",
	})
	if err != nil {
		t.Fatalf("ObjectBuildFile() failed: %v", err)
	}
	if strings.Contains(string(objectBuild), `objtool = "//linux:objtool"`) {
		t.Fatalf("arm64 object BUILD unexpectedly contains x86 objtool:\n%s", objectBuild)
	}
}

func TestCompactObjectBuildFileEmitsSourceInputs(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	sourceRoot := t.TempDir()
	writeCompactSource(t, sourceRoot, "init.c")
	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "base", Flags: nil},
	}, CompactMetadataOptions{Schema: CompactSchemaV012, SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}

	objectBuild, err := metadata.ObjectBuildFile(CompactBuildFileOptions{
		Schema:                  CompactSchemaV012,
		SourceLabelPackage:      "@linux//",
		SourceRootLabel:         "@linux//:Kconfig",
		SourceTreeAllFiles:      []string{"@linux//:all"},
		SourceTreeHeaders:       []string{"@linux//:headers"},
		SourceTreeLocalIncludes: []string{"@linux//:local_includes"},
		SourceTreeLookupFiles:   []string{"@linux//:lookup"},
		GeneratedHeaders:        "//linux:generated_headers",
	})
	if err != nil {
		t.Fatalf("ObjectBuildFile() failed: %v", err)
	}
	for _, want := range []string{
		`linux_source_tree(`,
		`root = "@linux//:Kconfig"`,
		`all_files = ["@linux//:all"]`,
		`headers = ["@linux//:headers"]`,
		`local_include_files = ["@linux//:local_includes"]`,
		`lookup_files = ["@linux//:lookup"]`,
		`source_tree_info = ":_source_tree"`,
		`generated_headers = "//linux:generated_headers"`,
	} {
		if !strings.Contains(string(objectBuild), want) {
			t.Fatalf("object BUILD missing %s:\n%s", want, objectBuild)
		}
	}
}

func TestCompactObjectBuildFileEmitsLiteralIncludeClosure(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader("obj-y += init.o entry.o dynamic.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "init.c", `
#include "fragments/first.inc"
#include "private/first.h"
#include <linux/global.h>
#include <asm/arch.h>
`)
	mustWriteSource(t, sourceRoot, "fragments/first.inc", `#include "../shared/second.inc"`+"\n")
	mustWriteSource(t, sourceRoot, "private/first.h", `#include "nested/second.h"`+"\n")
	mustWriteSource(t, sourceRoot, "private/nested/second.h", "int private_second;\n")
	mustWriteSource(t, sourceRoot, "include/linux/global.h", "int global;\n")
	mustWriteSource(t, sourceRoot, "arch/x86/include/asm/arch.h", "int arch;\n")
	mustWriteSource(t, sourceRoot, "entry.S", `#include "asm/entry.inc"`+"\n")
	mustWriteSource(t, sourceRoot, "asm/entry.inc", `#include "../shared/second.inc"`+"\n")
	mustWriteSource(t, sourceRoot, "dynamic.c", `#include DYNAMIC_HEADER`+"\n")
	mustWriteSource(t, sourceRoot, "shared/second.inc", "int second;\n")
	mustWriteSource(t, sourceRoot, "shared/unrelated.inc", "int unrelated;\n")

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "base", Flags: nil},
	}, CompactMetadataOptions{
		Schema:                          CompactSchemaV012,
		SourceRoot:                      sourceRoot,
		SourceGeneratedIncludesComplete: true,
		Srcarch:                         "x86",
	})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}
	config := configByName(metadata, "base")
	variants := map[string]CompactObjectVariant{}
	for object, wantIncludes := range map[string][]string{
		"entry.o": {"asm/entry.inc", "shared/second.inc"},
		"init.o": {
			"arch/x86/include/asm/arch.h",
			"fragments/first.inc",
			"include/linux/global.h",
			"private/first.h",
			"private/nested/second.h",
			"shared/second.inc",
		},
	} {
		variant := variantByTarget(metadata, objectTarget(metadata, config, object))
		if !reflect.DeepEqual(variant.SourceIncludes, wantIncludes) {
			t.Fatalf("%s SourceIncludes = %v, want %v", object, variant.SourceIncludes, wantIncludes)
		}
		if !variant.SourceIncludesComplete {
			t.Fatalf("%s literal include closure is marked incomplete", object)
		}
		variants[object] = variant
	}
	dynamic := variantByTarget(metadata, objectTarget(metadata, config, "dynamic.o"))
	if dynamic.SourceIncludesComplete {
		t.Fatal("dynamic.o computed include closure is marked complete")
	}
	variants["dynamic.o"] = dynamic

	objectBuild, err := metadata.ObjectBuildFile(CompactBuildFileOptions{
		Schema:                  CompactSchemaV012,
		SourceLabelPackage:      "@linux//",
		SourceRootLabel:         "@linux//:Kconfig",
		SourceTreeArchHeaders:   []string{"@linux//:x86_headers"},
		SourceTreeGlobalHeaders: []string{"@linux//:global_headers"},
		Srcarch:                 "x86",
	})
	if err != nil {
		t.Fatalf("ObjectBuildFile() failed: %v", err)
	}
	parsed, err := build.ParseBuild("objects.BUILD.bazel", objectBuild)
	if err != nil {
		t.Fatalf("generated object BUILD did not parse: %v\n%s", err, objectBuild)
	}
	for object, wantIncludes := range map[string][]string{
		"entry.o": {"@linux//:asm/entry.inc", "@linux//:shared/second.inc"},
		"init.o": {
			"@linux//:fragments/first.inc",
			"@linux//:private/first.h",
			"@linux//:private/nested/second.h",
			"@linux//:shared/second.inc",
		},
	} {
		rule := parsed.RuleNamed(variants[object].Target)
		if rule == nil {
			t.Fatalf("generated object BUILD missing %q:\n%s", variants[object].Target, objectBuild)
		}
		if got := rule.AttrStrings("source_includes"); !reflect.DeepEqual(got, wantIncludes) {
			t.Fatalf("%s source_includes = %v, want %v", object, got, wantIncludes)
		}
		if got := rule.AttrLiteral("source_includes_complete"); got != "True" {
			t.Fatalf("%s source_includes_complete = %q, want True", object, got)
		}
	}
	dynamicRule := parsed.RuleNamed(variants["dynamic.o"].Target)
	if dynamicRule == nil {
		t.Fatalf("generated object BUILD missing %q:\n%s", variants["dynamic.o"].Target, objectBuild)
	}
	if got := dynamicRule.AttrLiteral("source_includes_complete"); got != "" {
		t.Fatalf("dynamic.o source_includes_complete = %q, want omitted fallback", got)
	}
}

func TestSourceIncludeCoveredByClasses(t *testing.T) {
	opts := CompactBuildFileOptions{
		SourceTreeArchHeaders:   []string{"@linux//:x86_headers"},
		SourceTreeGlobalHeaders: []string{"@linux//:global_headers"},
		Srcarch:                 "x86",
	}
	for source, want := range map[string]bool{
		"arch/arm64/include/asm/page.h": false,
		"arch/x86/include/asm/page.h":   true,
		"drivers/net/private.h":         false,
		"include/linux/private.h":       true,
		"include/linux/source.inc":      false,
	} {
		if got := sourceIncludeCoveredByClasses(opts, source); got != want {
			t.Errorf("sourceIncludeCoveredByClasses(%q) = %v, want %v", source, got, want)
		}
	}
	if sourceIncludeCoveredByClasses(CompactBuildFileOptions{Srcarch: "x86"}, "include/linux/private.h") {
		t.Fatal("source include was filtered without a matching source-tree class")
	}
}

func TestCompactObjectVariantMissingCompletenessFailsClosed(t *testing.T) {
	var variant CompactObjectVariant
	if err := json.Unmarshal([]byte(`{
		"target": "init",
		"object": "init.o",
		"source": "init.c",
		"mode": "y"
	}`), &variant); err != nil {
		t.Fatalf("json.Unmarshal() failed: %v", err)
	}
	if variant.SourceIncludesComplete {
		t.Fatal("legacy compact metadata without completeness was treated as complete")
	}
}

func TestCompactImageBuildFileAliasesDuplicateObjectSets(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{
		{Name: "base", Flags: nil},
		{Name: "copy", Flags: nil},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	imageBuild, err := metadata.ImageBuildFile(CompactImageBuildFileOptions{})
	if err != nil {
		t.Fatalf("ImageBuildFile() failed: %v", err)
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

func TestCompactMetadataPreservesFlagOrder(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader(`
obj-y += init.o
ccflags-y += -UCONFIG_NET -DCONFIG_NET=1
`), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base", Flags: nil}})
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
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{{Name: "base", Flags: nil}})
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

func TestCompactMetadataRejectsDuplicateImageTargets(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	_, err := tree.CompactMetadata(kb, []NamedConfig{
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
	kb, err := ParseKbuild(strings.NewReader(fmt.Sprintf(`
obj-y += lib/crypto/sha256.o
ccflags-y += -I%s/lib/crypto/x86 -include %s/include/linux/hidden.h
`, sourceRoot, sourceRoot)), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "base", Flags: nil},
	}, CompactMetadataOptions{SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
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
		ConfigFragment: map[string]string{"CONFIG_NET": "y", "CONFIG_PCI": "n"},
	}
	if !ready.sourceBuildReady() {
		t.Fatalf("sourceBuildReady() rejected config-only make refs")
	}

	unresolved := CompactObjectVariant{
		Source:         "version.c",
		Flags:          []string{"-include", "$(obj)/utsversion-tmp.h"},
		ConfigFragment: map[string]string{},
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

func TestCompactObjectBuildFileRejectsUnbuildableLeaf(t *testing.T) {
	metadata := &CompactMetadata{
		ObjectVariants: []CompactObjectVariant{{
			Target: "broken",
			Object: "broken.o",
			Flags:  []string{"-I$(unsupported_dir)"},
			Source: "broken.c",
		}},
	}
	_, err := metadata.ObjectBuildFile(CompactBuildFileOptions{
		Schema:             CompactSchemaV012,
		SourceLabelPackage: "//linux",
		SourceRootLabel:    "//linux:Kconfig",
	})
	if err == nil {
		t.Fatal("ObjectBuildFile() unexpectedly accepted an unresolved source flag")
	}
	for _, want := range []string{"broken", "broken.o", "unsupported_dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ObjectBuildFile() error %q missing %q", err, want)
		}
	}
}

func TestSourceCandidatesForGeneratedArm64Objects(t *testing.T) {
	tests := map[string][]string{
		"arch/arm64/kernel/pi/lib-fdt.pi.o": {
			"lib/fdt.c",
			"arch/arm64/kernel/pi/fdt.c",
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
