// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kconfig

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	metadata, err := tree.CompactMetadata(kb, []NamedConfig{
		{Name: "base", Flags: map[string]string{"CONFIG_NET": "y"}},
	})
	if err != nil {
		t.Fatalf("CompactMetadata() failed: %v", err)
	}

	objectBuild, err := metadata.ObjectBuildFile(CompactBuildFileOptions{RuleLoadLabel: "//rules:linux_objects.bzl"})
	if err != nil {
		t.Fatalf("ObjectBuildFile() failed: %v", err)
	}
	if _, err := build.ParseBuild("objects.BUILD.bazel", objectBuild); err != nil {
		t.Fatalf("object BUILD did not parse: %v\n%s", err, objectBuild)
	}

	if !strings.Contains(string(objectBuild), `load("//rules:linux_objects.bzl", "linux_object")`) {
		t.Fatalf("object BUILD does not use custom compact rule load label:\n%s", objectBuild)
	}

	imageBuild, err := metadata.ImageBuildFile(CompactImageBuildFileOptions{
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

func TestCompactObjectBuildFileEmitsSourceLabels(t *testing.T) {
	tree := mustParseCompactFixture(t)
	kb := mustParseKbuildFixture(t)
	sourceRoot := t.TempDir()
	writeCompactSource(t, sourceRoot, "subdir/init.c")
	writeCompactSource(t, sourceRoot, "subdir/net/core.c")

	metadata, err := tree.CompactMetadataWithOptions(kb, []NamedConfig{
		{Name: "base", Flags: map[string]string{"CONFIG_NET": "y"}},
	}, CompactMetadataOptions{ObjectDir: "subdir", SourceRoot: sourceRoot})
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

	objectBuild, err := metadata.ObjectBuildFile(CompactBuildFileOptions{SourceLabelPackage: "@linux//"})
	if err != nil {
		t.Fatalf("ObjectBuildFile() failed: %v", err)
	}
	for _, want := range []string{
		`load("@linux.bzl//internal:linux_objects.bzl", "linux_config", "linux_object")`,
		`linux_config(`,
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
	}, CompactMetadataOptions{SourceRoot: sourceRoot})
	if err != nil {
		t.Fatalf("CompactMetadataWithOptions() failed: %v", err)
	}

	objectBuild, err := metadata.ObjectBuildFile(CompactBuildFileOptions{
		SourceLabelPackage: "@linux//",
		SourceRootLabel:    "@linux//:Kconfig",
		SourceTreeLabels:   []string{"@linux//:all"},
		GeneratedHeaders:   "//linux:generated_headers",
	})
	if err != nil {
		t.Fatalf("ObjectBuildFile() failed: %v", err)
	}
	for _, want := range []string{
		`linux_source_tree(`,
		`root = "@linux//:Kconfig"`,
		`srcs = ["@linux//:all"]`,
		`source_tree_info = ":_source_tree"`,
		`generated_headers = "//linux:generated_headers"`,
	} {
		if !strings.Contains(string(objectBuild), want) {
			t.Fatalf("object BUILD missing %s:\n%s", want, objectBuild)
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
