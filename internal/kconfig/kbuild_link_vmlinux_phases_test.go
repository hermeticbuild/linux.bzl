package kconfig

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

// The two historical layouts differ in their linker flags, the separate LD
// info line, and the error trap. Both put the version and object writes on the
// near side of separate recursive Make boundaries.
func compactKbuildLinkVmlinuxFixture(withTrap bool) string {
	linker := `${LD} ${KBUILD_LDFLAGS} -r -o ${1} ${lds} ${objects}`
	linkInfo := ""
	traps := ""
	if withTrap {
		linker = `${LD} ${KBUILD_LDFLAGS} -r -o ${1} ${objects}`
		linkInfo = "info LD vmlinux.o\n"
		traps = `on_exit()
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
`
	}
	return `#!/bin/sh
# SPDX-License-Identifier: GPL-2.0
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
	` + linker + `
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
mksysmap()
{
	${CONFIG_SHELL} "${srctree}/scripts/mksysmap" ${1} ${2}
}
cleanup()
{
	rm -f vmlinux.o vmlinux
}
` + traps + `case "${KBUILD_VERBOSE}" in
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
` + linkInfo + `modpost_link vmlinux.o
objtool_link vmlinux.o
# check vmlinux.o for section mismatches
${MAKE} -f "${srctree}/scripts/Makefile.modpost" MODPOST_VMLINUX=1
info MODINFO modules.builtin.modinfo
${OBJCOPY} -j .modinfo -O binary vmlinux.o modules.builtin.modinfo
info GEN modules.builtin
tr '\0' '\n' < modules.builtin.modinfo | sed -n 's/^.*\.file=//p' > modules.builtin
vmlinux_link vmlinux "${kallsyms}" ${btf_vmlinux_bin_o}
mksysmap vmlinux System.map
echo "vmlinux: $0" > .vmlinux.d
`
}

func TestAnalyzeCompactKbuildLinkVmlinuxPhasesSourceSpans(t *testing.T) {
	for _, test := range []struct {
		name     string
		withTrap bool
	}{
		{name: "5.10-shaped", withTrap: true},
		{name: "5.15-shaped"},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := compactKbuildLinkVmlinuxFixture(test.withTrap)
			phases, found, err := AnalyzeCompactKbuildLinkVmlinuxPhases(content)
			if err != nil || !found {
				t.Fatalf("analyze source script: found=%t, err=%v", found, err)
			}
			if got, want := phases.SourceSHA256, fmt.Sprintf("%x", sha256.Sum256([]byte(content))); got != want {
				t.Fatalf("source digest = %q, want %q", got, want)
			}
			for _, phase := range []struct {
				name   string
				script string
				spans  []CompactKbuildLinkVmlinuxSourceSpan
			}{
				{"version", phases.VersionScript, phases.VersionSpans},
				{"object", phases.ObjectScript, phases.ObjectSpans},
				{"final", phases.FinalScript, phases.FinalSpans},
			} {
				var reconstructed strings.Builder
				previous := 0
				for _, span := range phase.spans {
					if span.Start < previous || span.End < span.Start || span.End > len(content) ||
						span.Start > 0 && content[span.Start-1] != '\n' ||
						span.End > 0 && content[span.End-1] != '\n' {
						t.Fatalf("%s has invalid source span %+v", phase.name, span)
					}
					reconstructed.WriteString(content[span.Start:span.End])
					previous = span.End
				}
				if got := reconstructed.String(); got != phase.script {
					t.Fatalf("%s script differs from selected immutable source bytes", phase.name)
				}
			}
			if strings.Count(phases.VersionScript, "echo $VERSION > .version") != 1 ||
				strings.Contains(phases.ObjectScript, "echo $VERSION > .version") ||
				strings.Contains(phases.FinalScript, "echo $VERSION > .version") {
				t.Fatal("version increment executes outside its own phase")
			}
			if strings.Count(phases.ObjectScript, "modpost_link vmlinux.o\n") != 1 ||
				strings.Contains(phases.FinalScript, "modpost_link vmlinux.o\n") ||
				strings.Contains(phases.VersionScript, "modpost_link vmlinux.o\n") {
				t.Fatal("object link executes outside its own phase")
			}
			if strings.Contains(phases.VersionScript, "${MAKE} -f") ||
				strings.Contains(phases.ObjectScript, "${MAKE} -f") ||
				strings.Contains(phases.FinalScript, "${MAKE} -f") {
				t.Fatal("a script phase repeats a recursive Make boundary")
			}
			if !strings.Contains(phases.FinalScript, `echo "vmlinux: $0" > .vmlinux.d`) {
				t.Fatal("final phase lost the source script path used as shell $0")
			}

		})
	}
}

func TestAnalyzeCompactKbuildLinkVmlinuxPhasesRejectsPartialSourceShape(t *testing.T) {
	const initMake = `${MAKE} -f "${srctree}/scripts/Makefile.build" obj=init need-builtin=1`
	const modpostMake = `${MAKE} -f "${srctree}/scripts/Makefile.modpost" MODPOST_VMLINUX=1`
	content := compactKbuildLinkVmlinuxFixture(false)
	for _, test := range []struct {
		name string
		old  string
		new  string
	}{
		{"prelude side effect", ". include/config/auto.conf\n", ". include/config/auto.conf\ntouch unexpected\n"},
		{"dynamic linker initialization", `LD="$1"`, `LD="$(touch unexpected)"`},
		{"extra version update", "fi;\n", "fi;\necho 42 > .version\n"},
		{"new version arithmetic", "VERSION=$(expr 0$(cat .version) + 1)", "VERSION=$(expr 0$(cat .version) + 2)"},
		{"extra object effect", "objtool_link vmlinux.o\n", "touch unexpected\nobjtool_link vmlinux.o\n"},
		{"wrong linker output", "-r -o ${1} ${lds}", "-r -o other.o ${lds}"},
		{"altered init invocation", initMake, initMake + " EXTRA=1"},
		{"altered modpost invocation", modpostMake, modpostMake + " EXTRA=1"},
		{"additional recursive Make", "info MODINFO modules.builtin.modinfo\n", "${MAKE} -f extra\ninfo MODINFO modules.builtin.modinfo\n"},
		{"missing final link", `vmlinux_link vmlinux "${kallsyms}" ${btf_vmlinux_bin_o}`, `printf 'missing link\n'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			altered := strings.Replace(content, test.old, test.new, 1)
			if altered == content {
				t.Fatalf("test did not change source: %q", test.old)
			}
			_, found, err := AnalyzeCompactKbuildLinkVmlinuxPhases(altered)
			if err == nil || found {
				t.Fatalf("partial source shape accepted: found=%t, err=%v", found, err)
			}
		})
	}
}

func TestAnalyzeCompactKbuildLinkVmlinuxPhasesDistinguishesNewerSource(t *testing.T) {
	const script = `#!/bin/sh
set -e
${MAKE} -f "${srctree}/scripts/Makefile.build" obj=init init/version-timestamp.o
vmlinux_link vmlinux "${kallsyms}" ${btf_vmlinux_bin_o}
`
	_, found, err := AnalyzeCompactKbuildLinkVmlinuxPhases(script)
	if err != nil || found {
		t.Fatalf("newer source with no internal modpost boundary: found=%t, err=%v", found, err)
	}
}

func TestValidateCompactKbuildLinkVmlinuxAutoConf(t *testing.T) {
	for _, contents := range []string{
		"",
		"CONFIG_BPF=y\nCONFIG_SMP=n\nCONFIG_INIT_ENV_ARG_LIMIT=32\nCONFIG_BOOT_MAGIC=0x1234\n",
		`CONFIG_DEFAULT_HOSTNAME="(none)"` + "\n" + `CONFIG_ESCAPED="quote\" and backslash\\ and literal %%"` + "\n",
	} {
		if err := ValidateCompactKbuildLinkVmlinuxAutoConf(contents); err != nil {
			t.Fatalf("safe generated config %q: %v", contents, err)
		}
	}
	for _, test := range []struct {
		name     string
		contents string
	}{
		{"substitution in quoted value", `CONFIG_LOCALVERSION="$(touch side-effect)"` + "\n"},
		{"command substitution with backticks", "CONFIG_LOCALVERSION=\"`touch side-effect`\"\n"},
		{"escaped dollar", `CONFIG_LOCALVERSION="\$(touch side-effect)"` + "\n"},
		{"extra shell statement", "CONFIG_BPF=y; touch side-effect\n"},
		{"embedded newline", "CONFIG_NAME=\"one\ntwo\"\n"},
		{"wrong shell name", "CONFIG_NAME;touch=bad\n"},
		{"quoted suffix", "CONFIG_NAME=\"hello\"; touch side-effect\n"},
		{"unquoted shell expansion", "CONFIG_NAME=${PATH}\n"},
		{"unterminated source", "CONFIG_BPF=y"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateCompactKbuildLinkVmlinuxAutoConf(test.contents); err == nil {
				t.Fatalf("unsafe generated config accepted: %q", test.contents)
			}
		})
	}
}
