package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

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
	}, nil, nil)
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
	root := selectionRoleProfile(t, "all:\n", "all")
	root.Name = "root:deferred-exact-initial-frontier"

	modpost := selectionRoleProfileWithSources(t, `
all: scripts/mod/modpost
scripts/mod/empty.o: scripts/mod/empty.c
	$(CC) -c -o $@ $<
scripts/mod/elfconfig.h: scripts/mod/empty.o
	cp $< $@
scripts/mod/modpost.o: scripts/mod/modpost.c scripts/mod/elfconfig.h
	$(HOSTCC) -c -o $@ $<
scripts/mod/modpost: scripts/mod/modpost.o
	$(HOSTCC) -o $@ $<
`, map[string]string{
		"scripts/mod/empty.c":   "int empty;\n",
		"scripts/mod/modpost.c": "int main(void) { return 0; }\n",
	}, "all")
	modpost.Name = "child:deferred-unrelated-modpost"

	consumer := selectionRoleProfileWithSources(t, `
objtree := __LINUX_BZL_OBJECT_TREE__
export objtree
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
`, map[string]string{
		"arch/arm64/kernel/asm-offsets.c": "int asm_offsets;\n",
		"tools/consumer.c":                "int main(void) { return 0; }\n",
	}, "all")
	consumer.Name = "child:deferred-exact-consumer"
	consumer.InvocationPredecessors = []string{modpost.Name}
	setTestCompactKbuildInitialVisibleArtifacts(t, &consumer, []kconfig.CompactKbuildVisibleArtifact{{
		Path: "scripts/mod/modpost", Profile: modpost.Name, Target: "scripts/mod/modpost",
	}})
	control, err := kconfig.EvaluateSelectedKbuildControlEffects(consumer)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err = kconfig.AttachKbuildDeferredContentQueries(control.Profile, control)
	if err != nil {
		t.Fatal(err)
	}
	root.TargetInvocationDependencies = []kconfig.CompactKbuildInvocationDependency{
		{Target: "all", Profile: modpost.Name, Goals: modpost.EntryTargets},
		{Target: "all", Profile: consumer.Name, Goals: consumer.EntryTargets},
	}

	profiles := []kconfig.CompactKbuildProfile{root, modpost, consumer}
	selections := mustSelectedKbuildSelections(t, profiles, map[string]bool{
		"scripts/mod/empty.c":             true,
		"scripts/mod/modpost.c":           true,
		"arch/arm64/kernel/asm-offsets.c": true,
		"tools/consumer.c":                true,
	})
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
