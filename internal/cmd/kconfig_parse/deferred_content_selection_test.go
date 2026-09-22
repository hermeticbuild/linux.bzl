package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestSelectedKbuildCompressionQueryFollowsGeneratedAssemblyPrerequisites(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const directory = "arch/x86/boot/compressed"
	const compressed = directory + "/vmlinux.bin.lz4"
	write("Makefile", `
all:
	$(MAKE) -f $(srctree)/scripts/Makefile.build obj=arch/x86/boot/compressed arch/x86/boot/compressed/vmlinux
`)
	write("scripts/Makefile.build", `
objprefix := $(obj)
src := $(obj)
export CONFIG_SHELL := sh
export QUERY_CONTEXT := selected
LZ4 = ambient-lz4
CC = ambient-cc
squote := '
pound := \#
escsq = $(subst $(squote),'\$(squote)',$1)
cmd = @$(if $(cmd_$(1)),set -e; $(cmd_$(1)),:)
make-cmd = $(call escsq,$(subst $(pound),$$(pound),$(subst $$,$$$$,$(cmd_$(1)))))
dot-target = $(dir $@).$(notdir $@)
cmd_and_savecmd = $(cmd); printf '%s\n' 'savedcmd_$@ := $(make-cmd)' > $(dot-target).cmd
if-changed-cond = 1
if_changed = $(if $(if-changed-cond),$(cmd_and_savecmd),@:)
real-prereqs = $(filter-out FORCE,$^)
size_append = printf $(shell \
dec_size=0; \
for F in $(real-prereqs); do \
	fsize=$$($(CONFIG_SHELL) $(srctree)/scripts/file-size.sh $$F); \
	dec_size=$$(expr $$dec_size + $$fsize); \
done; \
printf "%08x\n" $$dec_size | \
sed 's/\(..\)/\1 /g' | { \
	read ch0 ch1 ch2 ch3; \
	for ch in $$ch3 $$ch2 $$ch1 $$ch0; do \
		printf '%s%03o' '\\' $$((0x$$ch)); \
	done; \
})
cmd_lz4 = { cat $(real-prereqs) | $(LZ4) -l -9 - -; $(size_append); } > $@
cmd_mkpiggy = $(obj)/mkpiggy $< > $@
include $(srctree)/arch/x86/boot/compressed/Makefile
$(objprefix)/%.o: $(src)/%.c FORCE
	$(CC) -c -o $@ $<
$(objprefix)/%.o: $(src)/%.S FORCE
	$(CC) -c -o $@ $<
`)
	write(directory+"/Makefile", `
$(objprefix)/vmlinux: $(objprefix)/piggy.o FORCE
	$(LD) -o $@ $<
$(objprefix)/mkpiggy: $(srctree)/arch/x86/boot/compressed/mkpiggy.c FORCE
	$(HOSTCC) -o $@ $<
$(objprefix)/piggy.S: $(objprefix)/vmlinux.bin.lz4 $(objprefix)/mkpiggy FORCE
	$(call if_changed,mkpiggy)
$(objprefix)/vmlinux.bin.lz4: $(objprefix)/vmlinux.bin FORCE
	$(call if_changed,lz4)
$(objprefix)/vmlinux.bin: input.bin FORCE
	cp $< $@
`)
	write("input.bin", "compressed input\n")
	write(directory+"/mkpiggy.c", "int main(void) { return 0; }\n")
	write("scripts/file-size.sh", "#!/bin/sh\n: \"$QUERY_CONTEXT\"\nwc -c < \"$1\"\n")
	variables := map[string]string{"SRCARCH": "x86"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithGeneratedContent(root, root,
		[]string{"all"}, nil, variables, kconfig.KbuildOptions{
			RootDir: root, Variables: variables,
			CommandLineVariables: map[string]string{
				"CC":     kconfig.KbuildActionRoleToken("target", "cc"),
				"LZ4":    kconfig.KbuildActionRoleToken("target", "lz4"),
				"LD":     kconfig.KbuildActionRoleToken("target", "ld"),
				"HOSTCC": kconfig.KbuildActionRoleToken("host", "cc"),
			},
			Shell: func(command string) (string, error) {
				return "", &kconfig.LinuxProbeOwnedUnsupportedCommandError{Architecture: "x86", Command: command}
			},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
		}, nil, nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{directory + "/piggy.o", directory + "/piggy.S", compressed, directory + "/vmlinux.bin"} {
		selectionByTarget(t, selections, target)
	}
	compressedSelection := selectionByTarget(t, selections, compressed)
	queries, err := kconfig.KbuildDeferredContentSelections(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || queries[0].Target != compressed {
		t.Fatalf("selected compressed content queries = %#v, want exactly one from %s", queries, compressed)
	}
	if got, want := compressedSelection.DeferredContentQueries,
		kconfig.EncodeCompactKbuildDeferredContentQueries([]string{queries[0].Token}); got != want {
		t.Fatalf("selected compressed target queries = %q, want %q", got, want)
	}
	var compressedProfile *kconfig.CompactKbuildProfile
	for index := range profiles {
		if profiles[index].Name == compressedSelection.Profile {
			compressedProfile = &profiles[index]
			break
		}
	}
	if compressedProfile == nil {
		t.Fatalf("selected compressed query profile %q is missing", compressedSelection.Profile)
	}
	snapshots := kconfig.CompactKbuildSelectedControlRecipeSnapshots(*compressedProfile, compressed)
	if len(snapshots) != 1 || snapshots[0] == nil {
		t.Fatalf("compressed recipe has %d selected line snapshots, want one", len(snapshots))
	}
	if got := snapshots[0].Environment["QUERY_CONTEXT"]; got != "selected" {
		t.Fatalf("selected recipe query environment = %q, want exported selected value", got)
	}
	effects, actionSelected, err := kconfig.EvaluateCompactKbuildSelectedTargetEffects(snapshots[0].Evaluation.Profile, compressed)
	if err != nil {
		t.Fatal(err)
	}
	if !actionSelected || len(effects.DeferredContentQueries) != 1 ||
		effects.DeferredContentQueries[0].Token != queries[0].Token {
		t.Fatalf("solver query and selected recipe disagree: selected %t, query count %d, want token %q", actionSelected, len(effects.DeferredContentQueries), queries[0].Token)
	}

	tree, err := kconfig.Parse(t.Context(), strings.NewReader("config TEST\n\tbool\n"), "Kconfig", kconfig.Options{})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := tree.CompactMetadataWithOptions(nil, kconfig.ResolveConfigOptions{},
		kconfig.CompactMetadataOptions{
			SelectedProductsOnly: true, // This fixture has no root vmlinux product facade.
			ActionRoles: []kconfig.KbuildActionRoleRef{
				{Scope: "target", Role: "cc"}, {Scope: "target", Role: "ld"},
				{Scope: "target", Role: "lz4"}, {Scope: "host", Role: "cc"},
			},
		}, func(*kconfig.ResolvedConfig) (kconfig.CompactConfigGraph, error) {
			return kconfig.CompactConfigGraph{
				KbuildProfiles: profiles, KbuildSelections: selections,
				KbuildDeferredContentSelections: queries,
			}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	identity := "sha256-" + strings.Repeat("5c", 32)
	if err := metadata.DiscoverActionPlanProbes(identity, identity); err != nil {
		t.Fatalf("probe discovery lost selected compression query %q: %v", queries[0].Token, err)
	}
	plan, err := metadata.ActionPlan(identity, identity)
	if err != nil {
		t.Fatalf("final action lowering lost selected compression query %q: %v", queries[0].Token, err)
	}
	queryActions := 0
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			if strings.HasPrefix(output.Path, ".linux-bzl-content/") {
				queryActions++
				if got := plan.Recipes[node.Recipe].Environment["QUERY_CONTEXT"]; got != "selected" {
					t.Fatalf("selected query action executes with QUERY_CONTEXT=%q, want exact exported value", got)
				}
			}
		}
	}
	if queryActions != 1 {
		t.Fatalf("final action plan has %d compression query actions, want one", queryActions)
	}
}

func TestSelectedKbuildDeferredExportPromotesExactOperandClosureForHostConsumers(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		filename := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("Makefile", `
export KBUILD_CFLAGS := -DBASE
export UNUSED_OBJECT_ROOT := __LINUX_BZL_OBJECT_TREE__
KBUILD_HOSTCFLAGS := -DHOST_BUILD

all: first-host second-host

first-host: prepare tools/first-host.c FORCE
	$(HOSTCC) $(KBUILD_HOSTCFLAGS) -o $@ tools/first-host.c
second-host: prepare tools/second-host.c FORCE
	$(HOSTCC) $(KBUILD_HOSTCFLAGS) -o $@ tools/second-host.c

prepare: stack_protector_prepare
stack_protector_prepare: prepare0
	$(eval KBUILD_CFLAGS += -mstack-protector-guard-offset=$(shell awk '{if ($$2 == "TSK_STACK_CANARY") print $$3;}' $(objtree)/include/generated/asm-offsets.h))

prepare0: include/generated/asm-offsets.h
include/generated/asm-offsets.h: arch/arm64/kernel/asm-offsets.c include/generated/bounds.h FORCE
	$(CC) -c -o $@ $<
include/generated/bounds.h: kernel/bounds.c FORCE
	$(CC) -c -o $@ $<
`)
	write("arch/arm64/kernel/asm-offsets.c", "int asm_offsets;\n")
	write("kernel/bounds.c", "int bounds;\n")
	write("tools/first-host.c", "int main(void) { return 0; }\n")
	write("tools/second-host.c", "int main(void) { return 0; }\n")

	variables := map[string]string{"SRCARCH": "arm64"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithGeneratedContent(root, root, []string{"all", "prepare"}, []string{"prepare"}, variables, kconfig.KbuildOptions{
		RootDir:   root,
		Variables: variables,
		CommandLineVariables: map[string]string{
			"CC":     kconfig.KbuildActionRoleToken("target", "cc"),
			"HOSTCC": kconfig.KbuildActionRoleToken("host", "cc"),
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
	}, nil, nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	asmOffsets := selectionByTarget(t, selections, "include/generated/asm-offsets.h")
	if asmOffsets.Scope != "target" || asmOffsets.Stage != "bootstrap" || asmOffsets.Lifecycle != "prep" {
		t.Fatalf("asm-offset selection = %#v, want prep-lifecycle target/bootstrap", asmOffsets)
	}
	bounds := selectionByTarget(t, selections, "include/generated/bounds.h")
	if bounds.Scope != "target" || bounds.Stage != "bootstrap" || bounds.Lifecycle != "prep" {
		t.Fatalf("asm-offset prerequisite selection = %#v, want prep-lifecycle target/bootstrap", bounds)
	}

	wantArtifact := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "include/generated/asm-offsets.h", Profile: asmOffsets.Profile, Target: asmOffsets.Target,
	}})
	querySelections, err := kconfig.KbuildDeferredContentSelections(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(querySelections), 1; got != want {
		t.Fatalf("deferred query selections = %#v, want %d", querySelections, want)
	}
	querySelection := querySelections[0]
	if querySelection.Target != "stack_protector_prepare" || querySelection.Scope != "target" ||
		querySelection.Stage != "bootstrap" || querySelection.Lifecycle != "prep" ||
		querySelection.GeneratedObjectTreeArtifacts != wantArtifact {
		t.Fatalf("deferred query selection = %#v, want prep target/bootstrap with exact asm-offset owner", querySelection)
	}
	profilesByName := make(map[string]kconfig.CompactKbuildProfile, len(profiles))
	for _, profile := range profiles {
		profilesByName[profile.Name] = profile
	}
	for _, target := range []string{"first-host", "second-host"} {
		host := selectionByTarget(t, selections, target)
		if host.Scope != "host" || host.Stage != "host" || host.Lifecycle != "target" {
			t.Errorf("%s selection = %#v, want target-lifecycle host/host", target, host)
		}
		if host.GeneratedObjectTreeArtifacts != "" {
			t.Errorf("%s consumer generated artifacts = %q, want query-owned frontier", target, host.GeneratedObjectTreeArtifacts)
		}
		if got, want := host.DeferredContentQueries, kconfig.EncodeCompactKbuildDeferredContentQueries([]string{querySelection.Token}); got != want {
			t.Errorf("%s deferred query selection = %q, want %q", target, got, want)
		}
		profile, ok := profilesByName[host.Profile]
		if !ok {
			t.Fatalf("%s references missing profile %q", target, host.Profile)
		}
		effects, selected, err := kconfig.EvaluateCompactKbuildSelectedTargetEffects(profile, target)
		if err != nil {
			t.Fatal(err)
		}
		if !selected || len(effects.DeferredContentQueries) != 1 {
			t.Fatalf("%s selected effects = %#v (selected %t), want one inherited deferred query", target, effects, selected)
		}
		query := effects.DeferredContentQueries[0]
		refs := kconfig.KbuildDeferredContentObjectTreeReferences(query.Command)
		if len(refs) != 1 || refs[0] != "include/generated/asm-offsets.h" {
			t.Fatalf("%s deferred query command = %q, extracted refs = %#v", target, query.Command, refs)
		}
		environment, err := kconfig.EvaluateCompactKbuildTargetEnvironment(
			profile, target, "", nil, nil, nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		flags := environment["KBUILD_CFLAGS"]
		if !strings.Contains(flags, "LINUX_BZL_KBUILD_CONTENT_") {
			t.Errorf("%s inherited KBUILD_CFLAGS = %q, want deferred bare-awk query", target, flags)
		}
	}
}

func TestSelectedKbuildDeferredExactOperandExcludesUnrelatedInitialHostClosure(t *testing.T) {
	root := t.TempDir()
	write := func(name, contents string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", `
all: modpost
	$(MAKE) -f $(srctree)/consumer.mk
modpost:
	$(MAKE) -f $(srctree)/modpost.mk
`)
	write("modpost.mk", `
all: scripts/mod/modpost
scripts/mod/empty.o: scripts/mod/empty.c
	$(CC) -c -o $@ $<
scripts/mod/elfconfig.h: scripts/mod/empty.o
	cp $< $@
scripts/mod/modpost.o: scripts/mod/modpost.c scripts/mod/elfconfig.h
	$(HOSTCC) -c -o $@ $<
scripts/mod/modpost: scripts/mod/modpost.o
	$(HOSTCC) -o $@ $<
`)
	write("consumer.mk", `
export KBUILD_CFLAGS := -DBASE
all: host-consumer
host-consumer: prepare tools/consumer.c
	$(HOSTCC) -o $@ tools/consumer.c
prepare: stack_protector_prepare
stack_protector_prepare: prepare0
	$(eval KBUILD_CFLAGS += -mstack-protector-guard-offset=$(shell awk '{if ($$2 == "TSK_STACK_CANARY") print $$3;}' $(objtree)/include/generated/asm-offsets.h))
prepare0: include/generated/asm-offsets.h
include/generated/asm-offsets.h: arch/arm64/kernel/asm-offsets.c
	$(CC) -c -o $@ $<
`)
	for _, name := range []string{"scripts/mod/empty.c", "scripts/mod/modpost.c", "arch/arm64/kernel/asm-offsets.c", "tools/consumer.c"} {
		write(name, "int input;\n")
	}
	variables := map[string]string{"SRCARCH": "arm64"}
	profiles, selections, _, err := evaluatedKbuildProfilesWithOptions(root, root, []string{"all"}, variables, kconfig.KbuildOptions{
		Variables:               variables,
		CommandLineVariables:    map[string]string{"CC": kconfig.KbuildActionRoleToken("target", "cc"), "HOSTCC": kconfig.KbuildActionRoleToken("host", "cc")},
		ConfigVariablesComplete: true, MakeVariablesComplete: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	querySelections, err := kconfig.KbuildDeferredContentSelections(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(querySelections), 1; got != want {
		t.Fatalf("deferred query selections = %#v, want %d", querySelections, want)
	}
	query := querySelections[0]
	asmOffsets := selectionByTarget(t, selections, "include/generated/asm-offsets.h")
	wantGenerated := kconfig.EncodeCompactKbuildInitialObjectTreeArtifacts([]kconfig.CompactKbuildVisibleArtifact{{
		Path: "include/generated/asm-offsets.h", Profile: asmOffsets.Profile, Target: asmOffsets.Target,
	}})
	if query.Target != "stack_protector_prepare" || query.Scope != "target" || query.Stage != "bootstrap" ||
		!query.UsesInitialObjectTree || query.InitialObjectTreeArtifacts != "" || query.GeneratedObjectTreeArtifacts != wantGenerated {
		t.Fatalf("deferred query frontier = %#v, want only exact generated asm-offsets owner %q", query, wantGenerated)
	}
	if strings.Contains(query.InitialObjectTreeArtifacts+query.GeneratedObjectTreeArtifacts, "modpost") {
		t.Fatalf("deferred query captured unrelated initial-visible modpost: %#v", query)
	}

	if got := selectionByTarget(t, selections, "scripts/mod/modpost"); got.Scope != "host" || got.Stage != "host" {
		t.Fatalf("unrelated modpost selection = %#v, want host/host", got)
	}
	if got := selectionByTarget(t, selections, "host-consumer"); got.Scope != "host" || got.Stage != "host" {
		t.Fatalf("deferred query consumer selection = %#v, want host/host", got)
	}
	if got := selectionByTarget(t, selections, "scripts/mod/empty.o"); got.Scope != "target" || got.Stage != "bootstrap" {
		t.Fatalf("modpost target prerequisite selection = %#v, want target/bootstrap", got)
	}
	if asmOffsets.Scope != "target" || asmOffsets.Stage != "bootstrap" {
		t.Fatalf("exact deferred operand selection = %#v, want target/bootstrap", asmOffsets)
	}
}
