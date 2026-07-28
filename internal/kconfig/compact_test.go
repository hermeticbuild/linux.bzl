package kconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
obj-m += trace.o helper.o
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

func TestCompactV013ObjectBuildUsesOneExactCompileEnvironmentIndex(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "init.c", "#include \"shared.h\"\n")
	mustWriteSource(t, sourceRoot, "shared.h", "#define SHARED 1\n")
	writeCompactSource(t, sourceRoot, "net/core.c")
	writeCompactSource(t, sourceRoot, "debug.c")
	writeCompactV013ForcedInputs(t, sourceRoot)

	common := CompactMetadataOptions{
		Schema:                CompactSchemaV013,
		SourceRoot:            sourceRoot,
		Srcarch:               "x86",
		CompileEnvironmentABI: "clang-21-linux-object-abi-v1",
	}
	baseOpts := common
	baseOpts.GeneratedHeadersLabel = "//headers:z_base"
	base, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{
		Name:  "base",
		Flags: map[string]string{"CONFIG_NET": "y"},
	}}, baseOpts)
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions(base) failed: %v", err)
	}
	debugOpts := common
	debugOpts.GeneratedHeadersLabel = "//headers:a_debug"
	debug, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{
		Name: "debug",
		Flags: map[string]string{
			"CONFIG_DEBUG": "y",
			"CONFIG_NET":   "y",
		},
	}}, debugOpts)
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions(debug) failed: %v", err)
	}
	metadata, err := MergeCompactMetadata(base, debug)
	if err != nil {
		t.Fatalf("MergeCompactMetadata() failed: %v", err)
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
		initVariant.SourceInputs,
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
	for _, redundant := range []string{`"source_inputs":`, `"config_fragment":`, `"fragment":`} {
		if strings.Contains(serialized, redundant) {
			t.Fatalf("v0.0.13 JSON retains redundant field %s:\n%s", redundant, metadataJSON)
		}
	}
	for _, required := range []string{`"source_files":`, `"source_input_groups":`, `"source_input_group":`} {
		if !strings.Contains(serialized, required) {
			t.Fatalf("v0.0.13 JSON omits indexed field %s:\n%s", required, metadataJSON)
		}
	}

	objectBuild, err := metadata.ObjectBuildFile(CompactBuildFileOptions{
		Schema:                   CompactSchemaV013,
		Arch:                     "x86",
		Version:                  "6.18.39",
		SourceLabelPackage:       "@linux//",
		SourceRootLabel:          "@linux//:Kconfig",
		SourceTreeAllFiles:       []string{"@linux//:all_files"},
		SourceTreeArchHeaders:    []string{"@linux//:arch_headers"},
		SourceTreeDtbSources:     []string{"@linux//:dtb_sources"},
		SourceTreeGlobalHeaders:  []string{"@linux//:global_headers"},
		SourceTreeHeaders:        []string{"@linux//:headers"},
		SourceTreeKbuildFiles:    []string{"@linux//:kbuild_files"},
		SourceTreeLocalIncludes:  []string{"@linux//:local_include_files"},
		SourceTreeLookupFiles:    []string{"@linux//:lookup_files"},
		SourceTreeScriptsHeaders: []string{"@linux//:scripts_headers"},
		SourceTreeUapiHeaders:    []string{"@linux//:uapi_headers"},
		Srcarch:                  "x86",
	})
	if err != nil {
		t.Fatalf("ObjectBuildFile() failed: %v", err)
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
			t.Errorf("v0.0.13 source tree unexpectedly emits broad %s", attr)
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
	for _, input := range metadata.SourceFiles {
		label := labelForSource(CompactBuildFileOptions{SourceLabelPackage: "@linux//"}, input.Path)
		if count := strings.Count(text, fmt.Sprintf("%q", label)); count != 1 {
			t.Fatalf("source label %q occurs %d times, want once", label, count)
		}
	}
	if strings.Contains(text, "linux_config(") {
		t.Fatalf("v0.0.13 object BUILD emitted per-object linux_config rules:\n%s", objectBuild)
	}
	if !strings.Contains(text, `"//headers:a_debug"`) || strings.Contains(text, `"//headers:z_base"`) {
		t.Fatalf("generated headers did not select the canonical shared label:\n%s", objectBuild)
	}
	initRule := parsed.RuleNamed(baseInit)
	if initRule == nil {
		t.Fatalf("generated object BUILD has no init rule %q:\n%s", baseInit, objectBuild)
	}
	for _, attr := range []string{"src", "source_includes", "source_includes_complete", "config_fragment"} {
		if initRule.Attr(attr) != nil {
			t.Fatalf("indexed init rule retains redundant %s:\n%s", attr, objectBuild)
		}
	}
	if got := initRule.AttrString("source_input_index"); got != ":_source_input_index" {
		t.Fatalf("init source_input_index = %q", got)
	}
	if got := initRule.AttrLiteral("source_input_group"); got != fmt.Sprintf("%d", initVariant.SourceInputGroup) {
		t.Fatalf("init source_input_group = %q, want %d", got, initVariant.SourceInputGroup)
	}
	sourceFile, err := metadata.sourceFileIndex(initVariant.Source)
	if err != nil {
		t.Fatal(err)
	}
	if got := initRule.AttrLiteral("source_input_file"); got != fmt.Sprintf("%d", sourceFile) {
		t.Fatalf("init source_input_file = %q, want %d", got, sourceFile)
	}
	if got := initRule.AttrString("compile_environment_index"); got != ":_compile_environment_index" {
		t.Fatalf("init compile_environment_index = %q", got)
	}
	if got := initRule.AttrString("compile_environment_id"); got != initVariant.CompileEnvironment {
		t.Fatalf("init compile_environment_id = %q, want %q", got, initVariant.CompileEnvironment)
	}
	if got := initRule.AttrString("content_id"); got != initVariant.ContentID {
		t.Fatalf("init content_id = %q, want %q", got, initVariant.ContentID)
	}
}

func TestCompactV013GeneratedHeaderFamiliesDeduplicateAcrossConfigs(t *testing.T) {
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
	writeCompactV013ForcedInputs(t, sourceRoot)
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
			Schema:                CompactSchemaV013,
			SourceRoot:            sourceRoot,
			Srcarch:               "x86",
			CompileEnvironmentABI: "object-abi-v1",
			KernelVersion:         "6.18.0",
		},
		func(config *ResolvedConfig) (*KbuildFile, string, error) {
			return kb, labels[config.Name], nil
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

func TestCompactV013Arm64GeneratedIncludeSelectsMonolithicFamily(t *testing.T) {
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
	writeCompactV013ForcedInputs(t, sourceRoot)
	metadata, err := tree.CompactMetadataWithOptions(
		kb,
		[]NamedConfig{{Name: "arm64"}},
		CompactMetadataOptions{
			Schema:                CompactSchemaV013,
			SourceRoot:            sourceRoot,
			Srcarch:               "arm64",
			CompileEnvironmentABI: "arm64-object-abi-v1",
			GeneratedHeadersLabel: "//headers:arm64",
		},
	)
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
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

func TestCompactMetadataBatchMatchesMergedConfigGraphs(t *testing.T) {
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
	writeCompactV013ForcedInputs(t, sourceRoot)

	configs := []NamedConfig{
		{Name: "debug", Flags: map[string]string{"CONFIG_DEBUG": "y"}},
		{Name: "base"},
	}
	opts := CompactMetadataOptions{
		Schema:                CompactSchemaV013,
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

	var singles []*CompactMetadata
	for _, config := range configs {
		configOpts := opts
		configOpts.GeneratedHeadersLabel = labels[config.Name]
		part, err := tree.CompactMetadataWithOptions(kbuilds[config.Name], []NamedConfig{config}, configOpts)
		if err != nil {
			t.Fatalf("CompactMetadataWithOptions(%s) failed: %v", config.Name, err)
		}
		singles = append(singles, part)
	}
	merged, err := MergeCompactMetadata(singles...)
	if err != nil {
		t.Fatalf("MergeCompactMetadata() failed: %v", err)
	}

	var calls []string
	batch, err := tree.CompactMetadataBatchWithOptions(configs, opts, func(config *ResolvedConfig) (*KbuildFile, string, error) {
		calls = append(calls, config.Name)
		return kbuilds[config.Name], labels[config.Name], nil
	})
	if err != nil {
		t.Fatalf("CompactMetadataBatchWithOptions() failed: %v", err)
	}
	if want := []string{"debug", "base"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("Kbuild callback calls = %v, want %v", calls, want)
	}
	gotJSON, err := batch.JSON()
	if err != nil {
		t.Fatalf("batch.JSON() failed: %v", err)
	}
	wantJSON, err := merged.JSON()
	if err != nil {
		t.Fatalf("merged.JSON() failed: %v", err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Fatalf("batch metadata differs from merged single-config metadata")
	}
	if got, want := batch.GeneratedHeaderFamilies[0].Labels, []string{"//headers:base", "//headers:debug"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("batch header labels = %v, want %v", got, want)
	}

	t.Run("callback error", func(t *testing.T) {
		_, err := tree.CompactMetadataBatchWithOptions(configs[:1], opts, func(*ResolvedConfig) (*KbuildFile, string, error) {
			return nil, "", fmt.Errorf("sentinel")
		})
		if err == nil || !strings.Contains(err.Error(), `resolve Kbuild for config "debug": sentinel`) {
			t.Fatalf("CompactMetadataBatchWithOptions() error = %v", err)
		}
	})
	t.Run("nil Kbuild", func(t *testing.T) {
		_, err := tree.CompactMetadataBatchWithOptions(configs[:1], opts, func(*ResolvedConfig) (*KbuildFile, string, error) {
			return nil, "", nil
		})
		if err == nil || !strings.Contains(err.Error(), `resolve Kbuild for config "debug": nil Kbuild`) {
			t.Fatalf("CompactMetadataBatchWithOptions() error = %v", err)
		}
	})
}

func TestCompactV013ContentIDsTrackOnlyTransitiveInputs(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader("obj-y += init.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "init.c", "#include \"shared.h\"\n")
	mustWriteSource(t, sourceRoot, "shared.h", "#define VALUE 1\n")
	writeCompactV013ForcedInputs(t, sourceRoot)
	generate := func() CompactObjectVariant {
		t.Helper()
		metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
			Schema:                CompactSchemaV013,
			SourceRoot:            sourceRoot,
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
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

func TestCompactV013ValidationRecomputesContentIDs(t *testing.T) {
	generate := func(t *testing.T) *CompactMetadata {
		t.Helper()
		tree := mustParseCompactFixture(t)
		kb, err := ParseKbuild(strings.NewReader("obj-y += init.o\n"), "Kbuild")
		if err != nil {
			t.Fatalf("ParseKbuild() failed: %v", err)
		}
		sourceRoot := t.TempDir()
		mustWriteSource(t, sourceRoot, "init.c", "int init_value;\n")
		writeCompactV013ForcedInputs(t, sourceRoot)
		metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
			Schema:                CompactSchemaV013,
			SourceRoot:            sourceRoot,
			Srcarch:               "x86",
			GeneratedHeadersLabel: "//headers:test",
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
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
			nil,
			"generated header family",
		)
		if err != nil {
			t.Fatal(err)
		}
		mutateDigest(t, metadata, inputs[0].Path)
		assertRejected(t, metadata, "generated header family")
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
	t.Run("dependency order", func(t *testing.T) {
		metadata := generate(t)
		root := metadata.ObjectVariants[0]
		inputs, err := metadata.expandedSourceInputGroup(root.SourceInputGroup, root.SourceInputs, "root")
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

func TestCompactV013SourceAndKernelFlagConfigsSplitCompileIdentity(t *testing.T) {
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
	writeCompactV013ForcedInputs(t, sourceRoot)
	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "base"},
		{Name: "source", Flags: map[string]string{"CONFIG_SOURCE_GATE": "y"}},
		{Name: "opt", Flags: map[string]string{"CONFIG_CC_OPTIMIZE_FOR_SIZE": "y"}},
	}, CompactMetadataOptions{
		Schema:                CompactSchemaV013,
		SourceRoot:            sourceRoot,
		CompileEnvironmentABI: "object-abi-v1",
	})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}

	variants := map[string]CompactObjectVariant{}
	for _, name := range []string{"base", "source", "opt"} {
		config := configByName(metadata, name)
		variants[name] = variantByTarget(metadata, objectTarget(metadata, config, "init.o"))
	}
	if got := variants["source"].ConfigFragment["CONFIG_SOURCE_GATE"]; got != "y" {
		t.Fatalf("source payload CONFIG_SOURCE_GATE = %q, want y", got)
	}
	if got := variants["opt"].ConfigFragment["CONFIG_CC_OPTIMIZE_FOR_SIZE"]; got != "y" {
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

func TestCompactV013GeneratedObjectActionFootprints(t *testing.T) {
	tests := []struct {
		object          string
		wantInput       string
		wantInclude     string
		wantConfig      string
		additionalFlags []string
	}{
		{"drivers/tty/vt/ucs.o", "drivers/tty/vt/ucs_width_table.h_shipped", "ucs_width_table.h", "", nil},
		{"drivers/scsi/scsi_sysfs.o", "include/scsi/scsi_devinfo.h", "scsi_devinfo_tbl.c", "", nil},
		{"lib/crc/crc32-main.o", "", "crc32table.h", "", nil},
		{"lib/crc32.o", "", "crc32table.h", "CONFIG_CRC32_SLICEBY4", nil},
		{"lib/crc/crc64-main.o", "", "crc64table.h", "", nil},
		{"lib/oid_registry.o", "include/linux/oid_registry.h", "oid_registry_data.c", "", nil},
		{"arch/x86/lib/inat.o", "arch/x86/lib/x86-opcode-map.txt", "inat-tables.c", "", nil},
		{"usr/initramfs_data.o", "usr/default_cpio_list", "", "", nil},
		{"arch/x86/kernel/cpu/capflags.o", "arch/x86/include/asm/cpufeatures.h", "", "", nil},
		{"arch/x86/realmode/rmpiggy.o", "", "pasyms.h", "", nil},
		{"init/version.o", "init/version-timestamp.c", "", "", nil},
		{"lib/fdt_ro.o", "scripts/dtc/libfdt/fdt_ro.c", "", "", nil},
		{"crypto/example.asn1.o", "scripts/asn1_compiler.c", "", "", nil},
		{"init/uts.o", "", "", "CONFIG_LOCALVERSION", []string{"-include", "$(obj)/utsversion-tmp.h"}},
	}
	for _, tc := range tests {
		t.Run(tc.object, func(t *testing.T) {
			got := compactObjectActionFootprintForObject(tc.object, tc.additionalFlags)
			if tc.wantInput != "" && !slices.Contains(got.sourceInputs, tc.wantInput) {
				t.Errorf("source inputs = %v, want %q", got.sourceInputs, tc.wantInput)
			}
			if tc.wantInclude != "" && !slices.Contains(got.providedIncludes, tc.wantInclude) {
				t.Errorf("provided includes = %v, want %q", got.providedIncludes, tc.wantInclude)
			}
			if tc.wantConfig != "" && !slices.Contains(got.configSymbols, tc.wantConfig) {
				t.Errorf("config symbols = %v, want %q", got.configSymbols, tc.wantConfig)
			}
		})
	}
}

func TestCompactV013ASN1GeneratedParserBindsEmittedHeaderClosures(t *testing.T) {
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
	writeCompactV013ForcedInputs(t, sourceRoot)

	generate := func() (CompactObjectVariant, []CompactSourceInput) {
		t.Helper()
		metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
			Schema:                CompactSchemaV013,
			SourceRoot:            sourceRoot,
			Srcarch:               "x86",
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
		}
		config := configByName(metadata, "base")
		variant := variantByTarget(metadata, objectTarget(metadata, config, "crypto/example.asn1.o"))
		inputs, err := metadata.expandedSourceInputGroup(
			variant.SourceInputGroup,
			variant.SourceInputs,
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

func TestCompactV013ASN1ConsumerRequiresResolvedParserObject(t *testing.T) {
	tree := mustParseCompactFixture(t)
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "crypto/consumer.c", "#include \"parser.asn1.h\"\n")
	mustWriteSource(t, sourceRoot, "crypto/parser.asn1", "Parser ::= INTEGER\n")
	mustWriteSource(t, sourceRoot, "scripts/asn1_compiler.c", "int asn1_compiler;\n")
	mustWriteSource(t, sourceRoot, "include/linux/asn1_ber_bytecode.h", "#define ASN1_BER 1\n")
	mustWriteSource(t, sourceRoot, "include/linux/asn1_decoder.h", "#define ASN1_DECODER 1\n")
	writeCompactV013ForcedInputs(t, sourceRoot)
	opts := CompactMetadataOptions{
		Schema:                CompactSchemaV013,
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
	metadata, err := tree.CompactMetadataWithOptions(withParser, []NamedConfig{{Name: "base"}}, opts)
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
	_, err = tree.CompactMetadataWithOptions(withoutParser, []NamedConfig{{Name: "base"}}, opts)
	if err == nil || !strings.Contains(err.Error(), "unresolved potentially-active literal include") {
		t.Fatalf("consumer without ASN.1 parser error = %v, want unresolved include", err)
	}
}

func TestCompactV013CapflagsIdentityUsesProducerHeaders(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb, err := ParseKbuild(strings.NewReader("obj-y += arch/x86/kernel/cpu/capflags.o\n"), "Kbuild")
	if err != nil {
		t.Fatalf("ParseKbuild() failed: %v", err)
	}
	sourceRoot := t.TempDir()
	mustWriteSource(t, sourceRoot, "arch/x86/kernel/cpu/mkcapflags.sh", "# nominal source\n")
	mustWriteSource(t, sourceRoot, "arch/x86/include/asm/cpufeatures.h", "#define X86_FEATURE_ONE 1\n")
	mustWriteSource(t, sourceRoot, "arch/x86/include/asm/vmxfeatures.h", "#define VMX_FEATURE_ONE 1\n")
	writeCompactV013ForcedInputs(t, sourceRoot)
	generate := func() CompactObjectVariant {
		t.Helper()
		metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{Name: "base"}}, CompactMetadataOptions{
			Schema:                CompactSchemaV013,
			SourceRoot:            sourceRoot,
			Srcarch:               "x86",
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
		}
		config := configByName(metadata, "base")
		return variantByTarget(metadata, objectTarget(metadata, config, "arch/x86/kernel/cpu/capflags.o"))
	}
	before := generate()
	mustWriteSource(t, sourceRoot, "arch/x86/include/asm/cpufeatures.h", "#define X86_FEATURE_TWO 2\n")
	after := generate()
	if before.ContentID == after.ContentID {
		t.Fatalf("capflags producer header change did not change content ID %q", before.ContentID)
	}
}

func TestObjectVariantContentIDUsesFullChildIDs(t *testing.T) {
	prefix := strings.Repeat("a", compactShortIDLength)
	left := prefix + strings.Repeat("b", 64-compactShortIDLength)
	right := prefix + strings.Repeat("c", 64-compactShortIDLength)
	leftID := objectVariantContentID("composite.o", "y", "", nil, nil, "", "", nil, nil, []string{left}, "abi-v1")
	rightID := objectVariantContentID("composite.o", "y", "", nil, nil, "", "", nil, nil, []string{right}, "abi-v1")
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
	)
	if got != want {
		t.Fatalf("objectVariantContentID() = %q, want canonical hash %q", got, want)
	}
}

func TestCompactV013CompositeIdentityIgnoresNonActionMetadata(t *testing.T) {
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
			[]string{"ignored.inc"},
			[]CompactSourceInput{{Path: "ignored.inc", Digest: strings.Repeat("f", 64)}},
			[]string{"member_target"},
			[]string{"ignored_dep"},
			[]string{strings.Repeat("a", 64)},
			[]string{strings.Repeat("b", 64)},
			"",
			nil,
			CompactSchemaV013,
			"linker-abi-v1",
			nil,
		)
	}
	left := variant("-DLEFT", "left")
	right := variant("-DRIGHT", "right")
	if left.ContentID != right.ContentID || left.Target != right.Target || !left.equal(right) {
		t.Fatalf("irrelevant composite metadata split identity:\nleft=%#v\nright=%#v", left, right)
	}
	if len(left.Flags) != 0 || len(left.SourceInputs) != 0 || len(left.Deps) != 0 || left.ModName != "" {
		t.Fatalf("v0.0.13 composite retained ignored action metadata: %#v", left)
	}
}

func TestCompactV013Arm64NvheIdentityBindsLinkerScriptAndConfig(t *testing.T) {
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
	writeCompactV013ForcedInputs(t, sourceRoot)
	generate := func(name string, flags map[string]string) (*CompactMetadata, CompactObjectVariant) {
		t.Helper()
		metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{
			Name:  name,
			Flags: flags,
		}}, CompactMetadataOptions{
			Schema:                CompactSchemaV013,
			SourceRoot:            sourceRoot,
			Srcarch:               "arm64",
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		return metadata, variantByTarget(metadata, objectTarget(metadata, config, "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o"))
	}
	_, off := generate("off", nil)
	onMetadata, on := generate("on", map[string]string{"CONFIG_NVHE_LAYOUT": "y"})
	if off.Object == "" || on.Object == "" {
		t.Fatalf("missing nVHE variants: off=%#v on=%#v", off, on)
	}
	if got := on.ConfigFragment["CONFIG_NVHE_LAYOUT"]; got != "y" {
		t.Fatalf("nVHE config fragment CONFIG_NVHE_LAYOUT = %q, want y", got)
	}
	if on.CompileEnvironment == off.CompileEnvironment || on.ContentID == off.ContentID {
		t.Fatalf("nVHE linker-script config did not split identity: off=%#v on=%#v", off, on)
	}
	onInputs, err := onMetadata.expandedSourceInputGroup(on.SourceInputGroup, on.SourceInputs, "nVHE")
	if err != nil {
		t.Fatalf("expand nVHE source inputs: %v", err)
	}
	if !slices.ContainsFunc(onInputs, func(input CompactSourceInput) bool {
		return input.Path == "arch/arm64/kvm/hyp/nvhe/hyp.lds.S"
	}) {
		t.Fatalf("nVHE source inputs omit hyp.lds.S: %v", onInputs)
	}
	objectBuild, err := onMetadata.ObjectBuildFile(CompactBuildFileOptions{
		Schema:             CompactSchemaV013,
		Arch:               "arm64",
		SourceLabelPackage: "@linux//",
		SourceRootLabel:    "@linux//:Kconfig",
		Srcarch:            "arm64",
	})
	if err != nil {
		t.Fatalf("ObjectBuildFile() failed: %v", err)
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

func TestCompactV013Arm64VDSO32WrapBindsNestedActionInputs(t *testing.T) {
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
		"arch/arm64/kernel/vdso32-wrap.S":            "\n",
		"arch/arm64/kernel/vdso32/note.c":            "int note;\n",
		"arch/arm64/kernel/vdso32/vdso.lds.S":        "SECTIONS { .text : { *(.text*) } }\n",
		"arch/arm64/kernel/vdso32/vgettimeofday.c":   "#include <asm/vdso/gettimeofday.h>\n",
		"lib/vdso/gettimeofday.c":                    "#include <asm/vdso/gettimeofday.h>\n",
	} {
		mustWriteSource(t, sourceRoot, path, content)
	}
	writeCompactV013ForcedInputs(t, sourceRoot)
	generate := func(name string) (*CompactMetadata, CompactObjectVariant) {
		t.Helper()
		metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{{Name: name}}, CompactMetadataOptions{
			Schema:                CompactSchemaV013,
			SourceRoot:            sourceRoot,
			Srcarch:               "arm64",
			CompileEnvironmentABI: "object-abi-v1",
		})
		if err != nil {
			t.Fatalf("CompactMetadataWithOptions(%s) failed: %v", name, err)
		}
		config := configByName(metadata, name)
		return metadata, variantByTarget(metadata, objectTarget(metadata, config, "arch/arm64/kernel/vdso32-wrap.o"))
	}

	metadata, before := generate("before")
	inputs, err := metadata.expandedSourceInputGroup(before.SourceInputGroup, before.SourceInputs, "compat vDSO")
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

func TestCompactV013SpecialSourceManifestExcludesHostTools(t *testing.T) {
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
	for _, value := range []string{string(CompactSchemaV011), string(CompactSchemaV012), string(CompactSchemaV013)} {
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
			nil,
			root,
			nil,
			CompactSchemaV013,
			"linux.bzl/compact-v5/test",
			nil,
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
		`src = "@linux//:subdir/init.c"`,
		`src = "@linux//:subdir/net/core.c"`,
	} {
		if !strings.Contains(string(objectBuild), want) {
			t.Fatalf("object BUILD missing %s:\n%s", want, objectBuild)
		}
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

func TestCompactObjectBuildFileEmitsQuotedIncludeClosure(t *testing.T) {
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

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "base", Flags: nil},
	}, CompactMetadataOptions{Schema: CompactSchemaV012, SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}
	config := configByName(metadata, "base")
	variants := map[string]CompactObjectVariant{}
	for object, wantIncludes := range map[string][]string{
		"entry.o": {"asm/entry.inc", "shared/second.inc"},
		"init.o":  {"fragments/first.inc", "shared/second.inc"},
	} {
		variant := variantByTarget(metadata, objectTarget(metadata, config, object))
		if !reflect.DeepEqual(variant.SourceIncludes, wantIncludes) {
			t.Fatalf("%s SourceIncludes = %v, want %v", object, variant.SourceIncludes, wantIncludes)
		}
		variants[object] = variant
	}

	objectBuild, err := metadata.ObjectBuildFile(CompactBuildFileOptions{
		Schema:             CompactSchemaV012,
		SourceLabelPackage: "@linux//",
		SourceRootLabel:    "@linux//:Kconfig",
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
		"init.o":  {"@linux//:fragments/first.inc", "@linux//:shared/second.inc"},
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

func TestCompactV013ImageBuildEmitsBaseRelativeDelta(t *testing.T) {
	id := func(value string) string {
		t.Helper()
		return strings.Repeat(value, 64)
	}
	metadata := &CompactMetadata{
		Schema: CompactSchemaV013,
		Configs: []CompactConfig{
			{
				Name:                "base",
				ImageTarget:         "base_image",
				ObjectTargets:       []string{"a", "b"},
				ModuleObjectTargets: []string{"m", "n"},
			},
			{
				Name:                "copy",
				ImageTarget:         "copy_image",
				ObjectTargets:       []string{"a", "b"},
				ModuleObjectTargets: []string{"m", "n"},
			},
			{
				Name:                "module_reorder",
				ImageTarget:         "module_reorder_image",
				ObjectTargets:       []string{"a", "b"},
				ModuleObjectTargets: []string{"n", "m"},
			},
			{
				Name:                "overlay",
				ImageTarget:         "overlay_image",
				ObjectTargets:       []string{"b", "c", "a"},
				ModuleObjectTargets: []string{"n"},
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
	imageBuild, err := metadata.ImageBuildFile(CompactImageBuildFileOptions{
		Schema:             CompactSchemaV013,
		Arch:               "x86",
		BaseConfig:         "base",
		ObjectLabelPackage: "//objects",
	})
	if err != nil {
		t.Fatalf("ImageBuildFile() failed: %v", err)
	}
	parsed, err := build.ParseBuild("images.BUILD.bazel", imageBuild)
	if err != nil {
		t.Fatalf("generated image BUILD did not parse: %v\n%s", err, imageBuild)
	}
	base := parsed.RuleNamed("base_image")
	if base == nil || base.Kind() != "linux_compact_image" {
		t.Fatalf("base image is not a linux_compact_image:\n%s", imageBuild)
	}
	if got, want := base.AttrStrings("objects"), []string{"//objects:a", "//objects:b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("base objects = %v, want %v", got, want)
	}
	if got, want := base.AttrStrings("module_objects"), []string{"//objects:m", "//objects:n"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("base module_objects = %v, want %v", got, want)
	}
	copyRule := parsed.RuleNamed("copy_image")
	if copyRule == nil || copyRule.Kind() != "alias" || copyRule.AttrString("actual") != ":base_image" {
		t.Fatalf("identical config did not alias the base image:\n%s", imageBuild)
	}
	moduleReorder := parsed.RuleNamed("module_reorder_image")
	if moduleReorder == nil || moduleReorder.Kind() != "linux_compact_delta_image" {
		t.Fatalf("module reorder with the same membership incorrectly aliased the base image:\n%s", imageBuild)
	}
	if got, want := moduleReorder.AttrStrings("ordered_module_content_ids"), []string{id("e"), id("d")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("module reorder ordered_module_content_ids = %v, want %v", got, want)
	}
	overlay := parsed.RuleNamed("overlay_image")
	if overlay == nil || overlay.Kind() != "linux_compact_delta_image" {
		t.Fatalf("overlay is not a linux_compact_delta_image:\n%s", imageBuild)
	}
	if got := overlay.AttrString("base_image"); got != ":base_image" {
		t.Fatalf("overlay base_image = %q, want :base_image", got)
	}
	if got, want := overlay.AttrStrings("add_objects"), []string{"//objects:c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay add_objects = %v, want %v", got, want)
	}
	if got, want := overlay.AttrStrings("remove_content_ids"), []string{id("d")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay remove_content_ids = %v, want %v", got, want)
	}
	if got, want := overlay.AttrStrings("ordered_content_ids"), []string{id("b"), id("c"), id("a")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay ordered_content_ids = %v, want built-ins only %v", got, want)
	}
	if got, want := overlay.AttrStrings("ordered_module_content_ids"), []string{id("e")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay ordered_module_content_ids = %v, want %v", got, want)
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

func writeCompactV013ForcedInputs(t *testing.T, root string) {
	t.Helper()
	for _, path := range []string{
		"include/linux/compiler-version.h",
		"include/linux/compiler_types.h",
		"include/linux/kconfig.h",
	} {
		mustWriteSource(t, root, path, "\n")
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
