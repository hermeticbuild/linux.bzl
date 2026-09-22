package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// Keep the Make boundary and its adjacent writes in source order: compile.h
// reads .version during the first child, before the parent writes vmlinux.o.
const sourcePhaseFrontierLinkScript = `#!/bin/sh
set -e
LD="$1"
KBUILD_LDFLAGS="$2"
LDFLAGS_vmlinux="$3"
info()
{
	printf "  %-7s %s\n" "${1}" "${2}"
}
modpost_link()
{
	local objects
	objects="${KBUILD_VMLINUX_OBJS} ${KBUILD_VMLINUX_LIBS}"
	${LD} ${KBUILD_LDFLAGS} -r -o ${1} ${objects}
}
objtool_link()
{
	if [ -n "${CONFIG_VMLINUX_VALIDATION}" ]; then
		tools/objtool/objtool check ${1}
	fi
}
vmlinux_link()
{
	${LD} ${KBUILD_LDFLAGS} -o ${1} ${KBUILD_VMLINUX_OBJS}
}
cleanup()
{
	rm -f vmlinux.o vmlinux
}
on_exit()
{
	if [ $? -ne 0 ]; then
		cleanup
	fi
}
trap on_exit EXIT
on_signals()
{
	exit 1
}
trap on_signals HUP INT QUIT TERM
case "${KBUILD_VERBOSE}" in
*1*)
	set -x
	;;
esac
if [ "$1" = "clean" ]; then
	cleanup
	exit 0
fi
. include/config/auto.conf
# Update version
info GEN .version
if [ -r .version ]; then
	VERSION=$(expr 0$(cat .version) + 1)
	echo $VERSION > .version
else
	rm -f .version
	echo 1 > .version
fi;
# final build of init/
${MAKE} -f "${srctree}/scripts/Makefile.build" obj=init need-builtin=1
# link vmlinux.o
info LD vmlinux.o
modpost_link vmlinux.o
objtool_link vmlinux.o
# check vmlinux.o for section mismatches
${MAKE} -f "${srctree}/scripts/Makefile.modpost" MODPOST_VMLINUX=1
info MODINFO modules.builtin.modinfo
vmlinux_link vmlinux "${kallsyms}" ${btf_vmlinux_bin_o}
echo "vmlinux: $0" > .vmlinux.d
`

func sourcePhaseFrontierFixture(t *testing.T, duplicateInvocation bool) (string, map[string]string) {
	t.Helper()
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
	const invoke = "\t$(cmd_vmlinux)\n"
	selected := invoke
	if duplicateInvocation {
		selected += invoke
	}
	write("Makefile", `
CONFIG_SHELL := sh
LD := ld
KBUILD_LDFLAGS := -z
LDFLAGS_vmlinux := defs
export MAKE srctree objtree LD KBUILD_LDFLAGS LDFLAGS_vmlinux
cmd_vmlinux = $(CONFIG_SHELL) $(srctree)/scripts/link-vmlinux.sh $(LD) $(KBUILD_LDFLAGS) $(LDFLAGS_vmlinux)
.PHONY: all
all: vmlinux
vmlinux: scripts/link-vmlinux.sh
`+selected)
	write("scripts/link-vmlinux.sh", sourcePhaseFrontierLinkScript)
	write("scripts/Makefile.build", `
.PHONY: __build FORCE
__build: init/built-in.a
init/built-in.a: include/generated/compile.h FORCE
	touch $@
include/generated/compile.h: .version scripts/mkcompile_h FORCE
	sh $(srctree)/scripts/mkcompile_h > $@
`)
	write("scripts/mkcompile_h", `#!/bin/sh
if [ -z "$KBUILD_BUILD_VERSION" ]; then
	VERSION=$(cat .version 2>/dev/null || echo 1)
else
	VERSION=$KBUILD_BUILD_VERSION
fi
printf '#define UTS_VERSION "%s"\n' "$VERSION"
`)
	write("scripts/Makefile.modpost", `
.PHONY: __modpost FORCE
__modpost: vmlinux.symvers
vmlinux.symvers: vmlinux.o FORCE
	touch $@
`)
	return root, map[string]string{
		"include/config/auto.conf": "CONFIG_LOCALVERSION=\"\"\nCONFIG_LOCALVERSION_AUTO=n\n",
	}
}

func sourcePhaseFrontierProfiles(t *testing.T, duplicateInvocation bool) ([]kconfig.CompactKbuildProfile, []kconfig.CompactKbuildSelection, string, error) {
	t.Helper()
	root, immutableContents := sourcePhaseFrontierFixture(t, duplicateInvocation)
	variables := linuxRootMakeInvocationVariables(root)
	variables["SRCARCH"] = "x86"
	return evaluatedKbuildProfilesWithGeneratedContent(
		root, root, []string{"all"}, nil, variables, kconfig.KbuildOptions{
			RootDir: root, Variables: variables,
			ConfigVariablesComplete: true, MakeVariablesComplete: true,
		}, nil, nil, immutableContents, nil, false,
	)
}

func TestSourcePhaseFrontierSelectsVersionBeforeNestedCompileHeader(t *testing.T) {
	profiles, selections, _, err := sourcePhaseFrontierProfiles(t, false)
	if err != nil {
		t.Fatal(err)
	}
	var parent, initChild, modpostChild *kconfig.CompactKbuildProfile
	for index := range profiles {
		switch profiles[index].Path {
		case "Makefile":
			parent = &profiles[index]
		case "scripts/Makefile.build":
			initChild = &profiles[index]
		case "scripts/Makefile.modpost":
			modpostChild = &profiles[index]
		}
	}
	if parent == nil || initChild == nil || modpostChild == nil {
		t.Fatalf("source-selected parent and children = %#v", profiles)
	}
	if len(parent.SelectedSourceScriptPhases) != 2 {
		t.Fatalf("selected script phases = %#v, want .version and vmlinux.o", parent.SelectedSourceScriptPhases)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(sourcePhaseFrontierLinkScript)))
	for _, expected := range []struct {
		output, kind string
		ordinal      int
	}{
		{output: ".version", kind: "version", ordinal: 0},
		{output: "vmlinux.o", kind: "object", ordinal: 1},
	} {
		phaseIndex := slices.IndexFunc(parent.SelectedSourceScriptPhases, func(phase kconfig.CompactKbuildSelectedSourcePhase) bool {
			return phase.OutputPath == expected.output
		})
		if phaseIndex < 0 {
			t.Fatalf("source writer for %q missing: %#v", expected.output, parent.SelectedSourceScriptPhases)
		}
		phase := parent.SelectedSourceScriptPhases[phaseIndex]
		selected := false
		for _, candidate := range selections {
			if candidate.Profile == parent.Name && candidate.Target == phase.OutputPath &&
				candidate.MakeTarget == phase.OutputPath && candidate.SourceScriptPhase == expected.kind {
				selected = true
				break
			}
		}
		if !selected || phase.OwnerTarget != "vmlinux" || phase.SourcePath != "scripts/link-vmlinux.sh" ||
			phase.Ordinal != expected.ordinal || phase.SourceSHA256 != digest || len(phase.Spans) == 0 ||
			!slices.Equal(phase.SourceArguments, []string{"ld", "-z", "defs"}) {
			t.Errorf("source-authenticated phase owner = %#v; selected=%t", phase, selected)
		}
	}
	wantVersion := kconfig.CompactKbuildVisibleArtifact{Path: ".version", Profile: parent.Name, Target: ".version"}
	if got, found := kconfig.CompactKbuildProfileInitialVisibleArtifact(*initChild, ".version"); !found || got != wantVersion {
		t.Fatalf("init child initially visible .version = %#v (found %t), want %#v", got, found, wantVersion)
	}
	compile := selectionByTarget(t, selections, "include/generated/compile.h")
	if compile.Profile != initChild.Name {
		t.Fatalf("nested compile.h owner = %q, want %q", compile.Profile, initChild.Name)
	}
	var native []kconfig.CompactKbuildVisibleArtifact
	if err := json.Unmarshal([]byte(compile.NativePrerequisiteArtifacts), &native); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(native, []kconfig.CompactKbuildVisibleArtifact{wantVersion}) {
		t.Fatalf("nested compile.h exact .version provenance = %#v, want %#v", native, wantVersion)
	}
	if _, found := kconfig.CompactKbuildProfileInitialVisibleArtifact(*initChild, "vmlinux.o"); found {
		t.Fatal("init child observed vmlinux.o before its source-script phase")
	}
	wantObject := kconfig.CompactKbuildVisibleArtifact{Path: "vmlinux.o", Profile: parent.Name, Target: "vmlinux.o"}
	if got, found := kconfig.CompactKbuildProfileInitialVisibleArtifact(*modpostChild, "vmlinux.o"); !found || got != wantObject {
		t.Fatalf("modpost child initially visible vmlinux.o = %#v (found %t), want %#v", got, found, wantObject)
	}
	for _, boundary := range []struct {
		profile, output string
	}{
		{initChild.Name, ".version"},
		{modpostChild.Name, "vmlinux.o"},
	} {
		found := false
		for _, dependency := range parent.TargetInvocationDependencies {
			if dependency.Target == "vmlinux" && dependency.Profile == boundary.profile &&
				dependency.SourcePhaseBefore == boundary.output {
				found = true
			}
		}
		if !found {
			t.Errorf("parent %s has no %q source phase boundary: %#v", boundary.profile, boundary.output,
				parent.TargetInvocationDependencies)
		}
	}
}

func TestSourcePhaseFrontierRejectsDuplicateVersionOwner(t *testing.T) {
	_, _, _, err := sourcePhaseFrontierProfiles(t, true)
	if err == nil || !strings.Contains(err.Error(), `both own ".version"`) {
		t.Fatalf("duplicate source-selected phase owner error = %v, want .version ownership conflict", err)
	}
}
