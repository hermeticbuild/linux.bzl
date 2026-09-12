package kconfig

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func compactKbuildProfileWithSourcesForTest(t *testing.T, profile CompactKbuildProfile, sources ...string) CompactKbuildProfile {
	t.Helper()
	root := t.TempDir()
	for _, source := range sources {
		mustWriteSource(t, root, source, "test source\n")
	}
	if profile.evaluator == nil || profile.evaluator.template == nil {
		t.Fatalf("profile %q has no captured evaluator", profile.Name)
	}
	roots := map[string]string{}
	for marker, existing := range profile.evaluator.template.sourceRoots {
		roots[marker] = existing
	}
	roots["__LINUX_BZL_SOURCE_TREE__"] = root
	profile.evaluator.template.sourceRoots = roots
	return profile
}

func compactGenericRecipeMetadataForTest(t *testing.T, recipe, separator string, targets ...string) (*CompactMetadata, string) {
	t.Helper()
	if len(targets) == 0 {
		t.Fatal("generic recipe test requires a target")
	}
	separatorText := ":"
	if separator == "&:" {
		separatorText = "&:"
	}
	kbuild := `
NM = ` + KbuildActionRoleToken("target", "nm") + `
cmd_transform = ` + recipe + `
` + strings.Join(targets, " ") + ` ` + separatorText + ` input.txt tools/filter FORCE
	$(call if_changed,transform)
`
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", kbuild, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.txt", "tools/filter")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	return &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}, targets[0]
}

func TestCompactKbuildRecipeWritesTargetUsesLoweredOutputEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		recipe string
		writes bool
	}{
		{name: "automatic touch", recipe: "touch $@", writes: true},
		{name: "copy destination", recipe: "cp input generated.stamp", writes: true},
		{name: "install destination", recipe: "install input generated.stamp", writes: true},
		{name: "install directory", recipe: "install -d generated.stamp", writes: false},
		{name: "install target directory", recipe: "install input --target-directory generated.stamp", writes: false},
		{name: "compiler output", recipe: "cc -c input.c -o generated.stamp", writes: true},
		{name: "configured joined compiler output", recipe: KbuildActionRoleToken("target", "cc") + " -ogenerated.stamp input.c", writes: true},
		{name: "configured compiler option terminator", recipe: KbuildActionRoleToken("target", "cc") + " -- -o generated.stamp", writes: false},
		{name: "configured Rust emit output", recipe: KbuildActionRoleToken("target", "rustc") + " --emit=dep-info=generated.d,link=generated.stamp input.rs", writes: true},
		{name: "stdout redirect", recipe: "printf data > generated.stamp", writes: true},
		{name: "if-changed generated text", recipe: "@set -e; trap 'rm -f generated.stamp; trap - HUP; kill -s HUP $$' HUP; trap 'rm -f generated.stamp; trap - INT; kill -s INT $$' INT; { echo data; :; } > generated.stamp; printf savedcmd > .generated.stamp.cmd", writes: true},
		{name: "mapped output", recipe: "cc -o ${tree:prep}/generated.stamp input.c", writes: true},
		{name: "padded explicit output", recipe: `cc -o " generated.stamp" input.c`, writes: false},
		{name: "quoted automatic touch", recipe: `touch "$@"`, writes: true},
		{name: "padded automatic touch", recipe: `touch " $@"`, writes: false},
		{name: "copy source", recipe: "cp generated.stamp copied.stamp", writes: false},
		{name: "diagnostic target", recipe: `printf '%s' "$@"`, writes: false},
		{name: "remove stale target", recipe: "rm -f $@", writes: false},
		{name: "directory target", recipe: "mkdir -p $@", writes: false},
		{name: "unrelated command", recipe: "echo setup", writes: false},
		{name: "different output", recipe: "cc -o other.stamp input.c", writes: false},
		{name: "unsupported shell", recipe: "echo setup || touch generated.stamp", writes: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := CompactKbuildRecipeWritesTarget(test.recipe, "generated.stamp"); got != test.writes {
				t.Fatalf("CompactKbuildRecipeWritesTarget(%q) = %t, want %t", test.recipe, got, test.writes)
			}
		})
	}
}

func TestCompactKbuildActionRootJoinsPreserveLeftTree(t *testing.T) {
	source := compactKbuildActionSourceTreeMarker
	object := compactKbuildActionObjectTreeMarker
	for _, test := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "object object", value: "-L" + object + "/" + object + "/rust", want: "-L" + object + "/rust"},
		{name: "source object", value: "-I" + source + "/" + object + "/arch/x86/boot", want: "-I" + source + "/arch/x86/boot"},
		{name: "object source", value: "-I" + object + "/" + source + "/generated", want: "-I" + object + "/generated"},
		{name: "nested", value: object + "/" + object + "/" + object + "/rust", want: object + "/rust"},
		{name: "terminal", value: "-I" + object + "/" + object, want: "-I" + object},
		{name: "public text", value: "${work:root}/${work:root}/rust", want: "${work:root}/${work:root}/rust"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := compactKbuildCollapseActionRootJoins(test.value); got != test.want {
				t.Fatalf("collapsed root join = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCompactKbuildRecipeLiteralOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		recipe string
		target string
		want   string
		ok     bool
	}{
		{
			name:   "automatic target",
			recipe: `echo '#include <asm-generic/termios.h>' > $@`,
			target: "include/generated/uapi/asm/termios.h",
			want:   "#include <asm-generic/termios.h>\n",
			ok:     true,
		},
		{name: "multiple arguments", recipe: `echo generated by Kbuild > generated.h`, target: "generated.h", want: "generated by Kbuild\n", ok: true},
		{name: "empty output line", recipe: `echo > generated.h`, target: "generated.h", want: "\n", ok: true},
		{name: "bin echo", recipe: `/bin/echo content > generated.h`, target: "generated.h", want: "content\n", ok: true},
		{name: "usr bin echo", recipe: `/usr/bin/echo content > generated.h`, target: "generated.h", want: "content\n", ok: true},
		{name: "literal later dash", recipe: `echo content -n > generated.h`, target: "generated.h", want: "content -n\n", ok: true},
		{name: "different target", recipe: `echo content > other.h`, target: "generated.h"},
		{name: "missing redirection", recipe: `echo content`, target: "generated.h"},
		{name: "append redirection", recipe: `echo content >> generated.h`, target: "generated.h"},
		{name: "environment", recipe: `LC_ALL=C echo content > generated.h`, target: "generated.h"},
		{name: "stdin", recipe: `echo content < input > generated.h`, target: "generated.h"},
		{name: "terminal connector", recipe: `echo content > generated.h;`, target: "generated.h"},
		{name: "multiple commands", recipe: `echo setup; echo content > generated.h`, target: "generated.h"},
		{name: "option", recipe: `echo -n content > generated.h`, target: "generated.h"},
		{name: "bin option", recipe: `/bin/echo -e content > generated.h`, target: "generated.h"},
		{name: "source executable", recipe: `tools/echo content > generated.h`, target: "generated.h"},
		{name: "relative executable", recipe: `./echo content > generated.h`, target: "generated.h"},
		{name: "normalized absolute executable", recipe: `/bin/../bin/echo content > generated.h`, target: "generated.h"},
		{name: "implementation defined escape", recipe: `echo 'content\nmore' > generated.h`, target: "generated.h"},
		{name: "opaque substitution", recipe: `echo "$(generator)" > generated.h`, target: "generated.h"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := CompactKbuildRecipeLiteralOutput(test.recipe, test.target)
			if got != test.want || ok != test.ok {
				t.Fatalf("CompactKbuildRecipeLiteralOutput(%q, %q) = (%q, %t), want (%q, %t)", test.recipe, test.target, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestCompactKbuildRecipeCommandLiteralOutputRejectsDynamicBindings(t *testing.T) {
	for _, value := range []string{
		"${tree:kernel}",
		"${work:root}",
		KbuildActionRoleToken("target", "cc"),
		compactKbuildActionSourceTreeMarker,
		compactKbuildLiteralTreeEscapeByte + "{tree:kernel}",
	} {
		t.Run(base64.RawURLEncoding.EncodeToString([]byte(value)), func(t *testing.T) {
			command := compactKbuildRecipeCommand{
				program: "echo", arguments: []string{value}, stdout: "generated.h",
			}
			if got, ok := compactKbuildRecipeCommandLiteralOutput(command, "generated.h"); ok || got != "" {
				t.Fatalf("dynamic echo payload %q lowered as literal %q", value, got)
			}
		})
	}
}

func TestCompactKbuildRecipeSourceProjection(t *testing.T) {
	for _, test := range []struct {
		name    string
		recipe  string
		target  string
		sources []string
		want    string
	}{
		{
			name: "direct copy", recipe: "cp source.h generated/include/source.h",
			target: "generated/include/source.h", sources: []string{"source.h"}, want: "source.h",
		},
		{
			name: "install into directory", recipe: "install -m 644 source.h generated/include",
			target: "generated/include/source.h", sources: []string{"source.h"}, want: "source.h",
		},
		{
			name: "install with option after source", recipe: "install source.h -m 644 generated/include",
			target: "generated/include/source.h", sources: []string{"source.h"}, want: "source.h",
		},
		{
			name: "absolute runtime install", recipe: "/usr/bin/install source.h --mode=644 generated/include",
			target: "generated/include/source.h", sources: []string{"source.h"}, want: "source.h",
		},
		{
			name: "cat projection", recipe: "cat source.h > generated/include/source.h",
			target: "generated/include/source.h", sources: []string{"source.h"}, want: "source.h",
		},
		{
			name: "diagnostic before projection", recipe: "printf '%s' generated/include/source.h; install -m 644 source.h generated/include",
			target: "generated/include/source.h", sources: []string{"source.h"}, want: "source.h",
		},
		{
			name: "stripping install", recipe: "install -s source.h generated/include",
			target: "generated/include/source.h", sources: []string{"source.h"},
		},
		{
			name: "attributes only copy", recipe: "cp --attributes-only source.h generated/include/source.h",
			target: "generated/include/source.h", sources: []string{"source.h"},
		},
		{
			name: "basename mismatch", recipe: "install -m 644 source.h generated/include",
			target: "generated/include/renamed.h", sources: []string{"source.h"},
		},
		{
			name: "ambiguous sources", recipe: "cp first.h second.h generated/include/source.h",
			target: "generated/include/source.h", sources: []string{"first.h", "second.h"},
		},
		{
			name: "later target rewrite", recipe: "cp source.h generated/include/source.h; cc -o generated/include/source.h generated.c",
			target: "generated/include/source.h", sources: []string{"source.h", "generated.c"},
		},
		{
			name: "source executable named install", recipe: "tools/install source.h generated/include",
			target: "generated/include/source.h", sources: []string{"source.h"},
		},
		{
			name: "missing mode argument", recipe: "install source.h -m",
			target: "generated/include/source.h", sources: []string{"source.h"},
		},
		{
			name: "multiple copy sources", recipe: "cp first.h second.h generated/include/source.h",
			target: "generated/include/source.h", sources: []string{"first.h", "second.h"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := CompactKbuildRecipeSourceProjection(test.recipe, test.target, test.sources)
			if ok != (test.want != "") || got != test.want {
				t.Fatalf("CompactKbuildRecipeSourceProjection(%q) = (%q, %t), want (%q, %t)", test.recipe, got, ok, test.want, test.want != "")
			}
		})
	}
}

func TestCompactKbuildRecipeRejectsLinearShellGroup(t *testing.T) {
	const grouped = "{ echo leaf.dtb; :; } > dtbs-list"
	if _, err := parseCompactKbuildRecipe(grouped, compactKbuildAutomaticContext{target: "dtbs-list"}); err == nil || !strings.Contains(err.Error(), `shell reserved word "{"`) {
		t.Fatalf("linear brace-group parse error = %v", err)
	}
	if !compactKbuildRecipeTextHasShellGroup(grouped) {
		t.Fatal("unquoted brace group was not recognized as shared shell structure")
	}
	if compactKbuildRecipeTextHasShellGroup(`printf '%s' '{' > dtbs-list`) {
		t.Fatal("quoted brace payload was misclassified as shell structure")
	}
}

func TestLexCompactKbuildRecipePreservesPOSIXDoubleQuotedBackslashes(t *testing.T) {
	tokens, err := lexCompactKbuildRecipe("printf \"generated/foo\\q.cmd\" \"literal\\\\slash\" \"literal\\$dollar\" \"joined\\\nline\"")
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token.operator {
			t.Fatalf("unexpected operator token %#v", token)
		}
		values = append(values, token.value)
	}
	want := []string{
		"printf",
		`generated/foo\q.cmd`,
		`literal\slash`,
		"literal" + compactKbuildLiteralDollarToken + "dollar",
		"joinedline",
	}
	if !slices.Equal(values, want) {
		t.Fatalf("double-quoted tokens=%q, want %q", values, want)
	}
}

func TestCompactKbuildRecipeUsesConfiguredPathArchive(t *testing.T) {
	archiver := KbuildActionRoleToken("target", "ar")
	for _, test := range []struct {
		name          string
		arguments     []string
		pathSensitive bool
	}{
		{name: "regular", arguments: []string{"rcD", "lib.a", "one.o"}},
		{name: "thin cluster", arguments: []string{"cDPrsT", "lib.a", "one.o"}, pathSensitive: true},
		{name: "thin dashed cluster", arguments: []string{"-rcST", "lib.a", "one.o"}, pathSensitive: true},
		{name: "thin split modifiers", arguments: []string{"-r", "-c", "-T", "lib.a", "one.o"}, pathSensitive: true},
		{name: "thin long option", arguments: []string{"--thin", "lib.a", "one.o"}, pathSensitive: true},
		{name: "thin long option after operation", arguments: []string{"rc", "--thin", "lib.a", "one.o"}, pathSensitive: true},
		{name: "thin operation after target option", arguments: []string{"--target=elf64-x86-64", "rcT", "lib.a", "one.o"}, pathSensitive: true},
		{name: "preserve paths", arguments: []string{"rcP", "lib.a", "one.o"}, pathSensitive: true},
		{name: "preserve paths split modifiers", arguments: []string{"-r", "-c", "-P", "lib.a", "one.o"}, pathSensitive: true},
		{name: "preserve paths long option", arguments: []string{"--full-paths", "lib.a", "one.o"}, pathSensitive: true},
		{name: "preserve paths long option after operation", arguments: []string{"rc", "--full-paths", "lib.a", "one.o"}, pathSensitive: true},
		{name: "preserve paths operation after plugin", arguments: []string{"--plugin", "/tools/plugin.so", "rcP", "lib.a", "one.o"}, pathSensitive: true},
		{name: "member after option terminator", arguments: []string{"rc", "--", "P-member.o"}},
		{name: "nested archiver capability", arguments: []string{archiver, "-r", "-c", "-T", "lib.a", "one.o"}, pathSensitive: true},
		{name: "nested archiver opaque mode", arguments: []string{"--archiver", archiver, "lib.a", "one.o"}, pathSensitive: true},
		{name: "generated archive wrapper", arguments: []string{"rcT", "lib.a", "one.o"}, pathSensitive: true},
		{name: "generated wrapper hidden mode", arguments: []string{"lib.a", "one.o"}, pathSensitive: true},
		{name: "bare non archive program", arguments: []string{"rcT", "lib.a", "one.o"}},
		{name: "unrelated tool", arguments: []string{"cDPrsT", "lib.a", "one.o"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			program := archiver
			switch test.name {
			case "nested archiver capability", "nested archiver opaque mode", "bare non archive program":
				program = "xargs"
			case "generated archive wrapper", "generated wrapper hidden mode":
				program = "scripts/ar-wrapper"
			case "unrelated tool":
				program = KbuildActionRoleToken("target", "ld")
			}
			commands := []compactKbuildRecipeCommand{{program: program, arguments: test.arguments}}
			if got := compactKbuildRecipeUsesConfiguredPathArchive(commands); got != test.pathSensitive {
				t.Fatalf("path-preserving archive detection=%t, want %t for %q", got, test.pathSensitive, test.arguments)
			}
		})
	}
}

func TestCompactKbuildRecipeRetainsArchiveMemberPaths(t *testing.T) {
	archiver := KbuildActionRoleToken("target", "ar")
	for _, test := range []struct {
		name      string
		program   string
		arguments []string
		retains   bool
	}{
		{name: "regular archive", program: archiver, arguments: []string{"rcD", "lib.a", "one.o"}},
		{name: "thin cluster", program: archiver, arguments: []string{"cDPrsT", "lib.a", "one.o"}, retains: true},
		{name: "thin split modifier", program: archiver, arguments: []string{"-r", "-c", "-T", "lib.a", "one.o"}, retains: true},
		{name: "thin long option", program: archiver, arguments: []string{"rc", "--thin", "lib.a", "one.o"}, retains: true},
		{name: "full paths only", program: archiver, arguments: []string{"rcP", "lib.a", "one.o"}},
		{name: "nested opaque archiver", program: "xargs", arguments: []string{"--archiver", archiver, "lib.a", "one.o"}, retains: true},
		{name: "generated wrapper with thin operation", program: "scripts/ar-wrapper", arguments: []string{"rcT", "lib.a", "one.o"}, retains: true},
		{name: "generated wrapper with hidden mode", program: "scripts/ar-wrapper", arguments: []string{"lib.a", "one.o"}},
		{name: "generated linker with split T option", program: "scripts/link-wrapper", arguments: []string{"-T", "module.lds", "one.o"}},
		{name: "unrelated configured tool", program: KbuildActionRoleToken("target", "ld"), arguments: []string{"rcT", "lib.a", "one.o"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			commands := []compactKbuildRecipeCommand{{program: test.program, arguments: test.arguments}}
			if got := compactKbuildRecipeRetainsArchiveMemberPaths(commands); got != test.retains {
				t.Fatalf("retained-member detection=%t, want %t for %q %q", got, test.retains, test.program, test.arguments)
			}
		})
	}
}

func TestCompoundProgramDiscoveryRetainsPathArchiveModeArguments(t *testing.T) {
	commands, err := compactKbuildCompoundProgramCommands(
		KbuildActionRoleToken("target", "ar") + " rc --thin lib.a one.o && true",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !compactKbuildRecipeUsesConfiguredPathArchive(commands) {
		t.Fatalf("compound commands lost path-preserving archive mode: %#v", commands)
	}
}

func TestCompoundProgramDiscoveryRetainsStaticRedirections(t *testing.T) {
	commands, err := compactKbuildCompoundProgramCommands(
		"generate < input.txt > output.txt; append >> state.cmd",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[0].stdin != "input.txt" || commands[0].stdout != "output.txt" ||
		commands[1].stdout != "state.cmd" {
		t.Fatalf("compound command redirections = %#v", commands)
	}
}

func TestCompoundWorkingInputCompletenessUsesShellQuoteProvenanceAndClosedSearchGrammar(t *testing.T) {
	profile := CompactKbuildProfile{Name: "compound-input-use-proof"}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	cc := KbuildActionRoleToken("host", "cc")
	ld := KbuildActionRoleToken("host", "ld")
	objectRoot := "__LINUX_BZL_OBJECT_TREE__/"
	for _, test := range []struct {
		name     string
		command  string
		complete bool
	}{
		{name: "direct source", command: cc + " -c source.c -o " + objectRoot + "out", complete: true},
		{name: "unquoted glob", command: cc + " source.c libs/*.a -o " + objectRoot + "out"},
		{name: "single quoted glob", command: cc + " source.c 'libs/*.a' -o " + objectRoot + "out", complete: true},
		{name: "double quoted glob", command: cc + ` source.c "libs/*.a" -o ` + objectRoot + "out", complete: true},
		{name: "escaped glob", command: cc + ` source.c libs/\*.a -o ` + objectRoot + "out", complete: true},
		{name: "unquoted tilde", command: cc + " source.c ~/lib.a -o " + objectRoot + "out"},
		{name: "quoted tilde", command: cc + " source.c '~/lib.a' -o " + objectRoot + "out", complete: true},
		{name: "environment tilde", command: "LIB=~/lib " + cc + " -c source.c -o " + objectRoot + "out"},
		{name: "environment search path", command: "CPATH=libs " + cc + " -c source.c -o " + objectRoot + "out"},
		{name: "parameter expansion", command: cc + " source.c $LIB -o " + objectRoot + "out"},
		{name: "braced parameter expansion", command: cc + ` source.c "${LIB}" -o ` + objectRoot + "out"},
		{name: "command substitution", command: cc + " source.c $(printf libs/lib.a) -o " + objectRoot + "out"},
		{name: "arithmetic expansion", command: cc + " source.c $((1)) -o " + objectRoot + "out"},
		{name: "single quoted parameter bytes", command: cc + " source.c '$LIB' -o " + objectRoot + "out", complete: true},
		{name: "escaped parameter bytes", command: cc + ` source.c \$LIB -o ` + objectRoot + "out", complete: true},
		{name: "stdin glob", command: cc + " -c source.c -o " + objectRoot + "out < inputs/*"},
		{name: "stdout glob", command: cc + " -c source.c -o " + objectRoot + "out > outputs/*"},
		{name: "stdout parameter expansion", command: cc + " -c source.c -o " + objectRoot + "out > $OUTPUT"},
		{name: "driver library search", command: cc + " source.c -L libs -lhidden -o " + objectRoot + "out"},
		{name: "driver linker script", command: cc + " source.c -Wl,-T,layout.lds -o " + objectRoot + "out"},
		{name: "driver sysroot", command: cc + " source.c --sysroot libs -o " + objectRoot + "out"},
		{name: "driver joined isysroot", command: cc + " source.c -isysrootlibs -o " + objectRoot + "out"},
		{name: "driver gcc toolchain", command: cc + " source.c --gcc-toolchain libs -o " + objectRoot + "out"},
		{name: "driver joined gcc toolchain", command: cc + " source.c --gcc-toolchain=libs -o " + objectRoot + "out"},
		{name: "relocatable exact target", command: ld + " -r -o " + objectRoot + ".tmp_out " + objectRoot + "out", complete: true},
		{name: "relocatable library search", command: ld + " -r -o " + objectRoot + ".tmp_out " + objectRoot + "out -L libs -lhidden"},
		{name: "relocatable script", command: ld + " -r -o " + objectRoot + ".tmp_out " + objectRoot + "out -T layout.lds"},
		{name: "objtool target only", command: objectRoot + "tools/objtool/objtool --noinstr " + objectRoot + "out", complete: true},
		{name: "objtool mcount", command: objectRoot + "tools/objtool/objtool --mcount " + objectRoot + "out", complete: true},
		{name: "objtool version booleans", command: objectRoot + "tools/objtool/objtool --ibt --unret --no-fp --cfi --noabs --backtrace --sec-address --verbose --Werror " + objectRoot + "out", complete: true},
		{name: "objtool backup side effect", command: objectRoot + "tools/objtool/objtool --backup " + objectRoot + "out"},
		{name: "objtool response", command: objectRoot + "tools/objtool/objtool @options " + objectRoot + "out"},
		{name: "objtool unknown option", command: objectRoot + "tools/objtool/objtool --search=libs " + objectRoot + "out"},
	} {
		t.Run(test.name, func(t *testing.T) {
			commands, err := compactKbuildCompoundProgramCommands(test.command)
			if err != nil {
				t.Fatal(err)
			}
			if got := compactKbuildCompoundWorkingInputUsesComplete(profile, "out", commands); got != test.complete {
				t.Fatalf("working-input completeness = %t, want %t for commands %#v", got, test.complete, commands)
			}
		})
	}
}

func TestCompoundProgramDiscoveryHandlesDescriptorRedirections(t *testing.T) {
	commands, err := compactKbuildCompoundProgramCommands(
		"echo >&2 diagnostic; tool input 2>/dev/null 3<input 4>&1 1>output; sink <&0; " +
			"printf 2 >&1; printf '2'>&1; printf \\2>&1; 2>&1 final ok; " +
			"discard >/dev/null; empty </dev/null",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 9 ||
		commands[0].program != "echo" || !slices.Equal(commands[0].arguments, []string{"diagnostic"}) ||
		commands[0].stdin != "" || commands[0].stdout != "" ||
		commands[1].program != "tool" || !slices.Equal(commands[1].arguments, []string{"input"}) ||
		commands[1].stdin != "" || commands[1].stdout != "output" ||
		commands[2].program != "sink" || commands[2].stdin != "" || commands[2].stdout != "" ||
		commands[3].program != "printf" || !slices.Equal(commands[3].arguments, []string{"2"}) ||
		commands[4].program != "printf" || !slices.Equal(commands[4].arguments, []string{"2"}) ||
		commands[5].program != "printf" || !slices.Equal(commands[5].arguments, []string{"2"}) ||
		commands[6].program != "final" || !slices.Equal(commands[6].arguments, []string{"ok"}) ||
		commands[7].program != "discard" || commands[7].stdin != "" || commands[7].stdout != "" ||
		commands[8].program != "empty" || commands[8].stdin != "" || commands[8].stdout != "" {
		t.Fatalf("descriptor-aware compound commands = %#v", commands)
	}
}

func TestCompoundProgramDiscoveryKeepsCommandSubstitutionProgramsNested(t *testing.T) {
	archiver := KbuildActionRoleToken("target", "ar")
	recipe := "rm -f vmlinux.a; " + archiver + " cDPrST vmlinux.a built-in.a; " +
		archiver + " mPiT $(" + archiver + " t vmlinux.a | sed -n 1p) vmlinux.a $(" +
		archiver + " t vmlinux.a | grep -F -f scripts/head-object-list.txt)"
	commands, err := compactKbuildCompoundProgramCommands(recipe)
	if err != nil {
		t.Fatal(err)
	}
	programs := make([]string, len(commands))
	for index, command := range commands {
		programs[index] = command.program
		if command.programStart < 0 || command.programEnd > len(recipe) ||
			recipe[command.programStart:command.programEnd] != command.program {
			t.Fatalf("command %d has invalid head provenance %#v in %q", index, command, recipe)
		}
	}
	want := []string{"rm", archiver, archiver, archiver, "sed", archiver, "grep"}
	if !slices.Equal(programs, want) {
		t.Fatalf("command substitution programs=%q, want %q (archive operands must not become programs)", programs, want)
	}
}

func TestObjectTreeCommandProgramsUseCommandHeadProvenance(t *testing.T) {
	profile := CompactKbuildProfile{}
	command := "MODE=probe ${tree:prep}/scripts/generated-tool input && " +
		"${tree:kernel}/scripts/source-tool input | bare-tool $(./nested-generated input); " +
		"${work:root}/arch/x86/boot/mkcpustr > output"
	programs, err := CompactKbuildObjectTreeCommandPrograms(profile, command)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"arch/x86/boot/mkcpustr", "nested-generated", "scripts/generated-tool"}
	if !slices.Equal(programs, want) {
		t.Fatalf("object-tree command programs=%q, want %q", programs, want)
	}
}

func TestObjectTreeCommandProgramsRejectDynamicCommandHead(t *testing.T) {
	if programs, err := CompactKbuildObjectTreeCommandPrograms(
		CompactKbuildProfile{}, "$(choose-tool) input",
	); err == nil {
		t.Fatalf("dynamic command programs=%q, want undecidable-command error", programs)
	}
}

func TestRecipeProducesNonIncludeOutputUsesConfiguredActionSemantics(t *testing.T) {
	cc := KbuildActionRoleToken("host", "cc")
	ar := KbuildActionRoleToken("host", "ar")
	for _, test := range []struct {
		name, recipe, target string
		want                 bool
	}{
		{
			name:   "compiler driver link",
			recipe: cc + " input.o -o ${work:root}/tools/generated",
			target: "tools/generated",
			want:   true,
		},
		{
			name:   "compiler object",
			recipe: cc + " -c input.c -o ${tree:prep}/generated/input.o",
			target: "generated/input.o",
			want:   true,
		},
		{
			name:   "compiler preprocessor data",
			recipe: cc + " -E input.c -o ${tree:prep}/generated/input.h",
			target: "generated/input.h",
		},
		{
			name:   "compiler assembly text",
			recipe: cc + " -S input.c -o ${tree:prep}/generated/input.s",
			target: "generated/input.s",
		},
		{
			name:   "attached primary output",
			recipe: cc + " -c input.c -o${tree:prep}/generated/input.o",
			target: "generated/input.o",
			want:   true,
		},
		{
			name:   "object with side dependency",
			recipe: cc + " -c -MMD -MF ${tree:prep}/generated/input.d -o ${tree:prep}/generated/input.o input.c",
			target: "generated/input.o",
			want:   true,
		},
		{
			name:   "dependency side output is not primary",
			recipe: cc + " -c -MMD -MF ${tree:prep}/generated/input.d -o ${tree:prep}/generated/input.o input.c",
			target: "generated/input.d",
		},
		{
			name:   "later text overwrite",
			recipe: cc + " -c input.c -o ${tree:prep}/generated/input.o; printf text > ${tree:prep}/generated/input.o",
			target: "generated/input.o",
		},
		{
			name:   "opaque in-place postprocessor",
			recipe: cc + " -c input.c -o ${tree:prep}/generated/input.o; ${tree:host}/tools/postprocess ${tree:prep}/generated/input.o",
			target: "generated/input.o",
			want:   true,
		},
		{
			name:   "temporary object move",
			recipe: cc + " -c input.c -o ${tree:prep}/generated/input.tmp; mv ${tree:prep}/generated/input.tmp ${tree:prep}/generated/input.o",
			target: "generated/input.o",
			want:   true,
		},
		{
			name:   "configured stdout is data",
			recipe: KbuildActionRoleToken("host", "ld") + " -M > ${tree:prep}/generated/link.map",
			target: "generated/link.map",
		},
		{
			name:   "mutating archive operand",
			recipe: ar + " cDPrST ${tree:prep}/generated/built-in.a input.o",
			target: "generated/built-in.a",
			want:   true,
		},
		{
			name:   "wrapper-carried mutating archive operand",
			recipe: "printf '%s ' input.o | xargs " + ar + " cDPrST ${tree:prep}/generated/built-in.a",
			target: "generated/built-in.a",
			want:   true,
		},
		{
			name:   "configured archive token passed as data",
			recipe: "printf '%s' " + ar + " cDPrST ${tree:prep}/generated/not-an-archive",
			target: "generated/not-an-archive",
		},
		{
			name:   "archive member extraction stdout",
			recipe: ar + " p input.a > ${tree:prep}/generated/member.txt",
			target: "generated/member.txt",
		},
		{
			name:   "archive table stdout",
			recipe: ar + " t input.a > ${tree:prep}/generated/table.txt",
			target: "generated/table.txt",
		},
		{
			name:   "unrelated output",
			recipe: cc + " input.o -o ${work:root}/tools/other",
			target: "tools/generated",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := CompactKbuildRecipeProducesNonIncludeOutput(CompactKbuildProfile{}, test.recipe, test.target)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("linked output=%t, want %t for %q", got, test.want, test.recipe)
			}
		})
	}
}

func TestRootCompoundArchiveProjectionUsesBareCanonicalPaths(t *testing.T) {
	const target = "vmlinux.a"
	archiver := KbuildActionRoleToken("target", "ar")
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.vmlinux", "", `
vmlinux.a: built-in.a lib/lib.a arch/x86/lib/lib.a FORCE
	rm -f $@; $(AR) cDPrST $@ built-in.a lib/lib.a arch/x86/lib/lib.a; $(AR) mPiT $$($(AR) t $@ | sed -n 1p) $@ $$($(AR) t $@ | grep -F init/main.o)
`, map[string]string{"AR": archiver})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	inputs := []compactKbuildRuleInput{}
	for _, pathname := range []string{"built-in.a", "lib/lib.a", "arch/x86/lib/lib.a"} {
		producer, err := appendActionPlanNode(plan, ActionPlanNode{
			Stage: "target", Kind: "archive", Tool: "ar", Product: "vmlinux",
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: pathname}},
		}, ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "archive", Tool: "ar",
			Arguments: []string{"cDPrST", "${output:00000000}"}, Outputs: []string{"00000000"},
		})
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, compactKbuildRuleInput{path: pathname, producer: producer})
	}
	match, matched, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatalf("target %q did not select its source rule", target)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, match, inputs)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok || node.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("root compound archive=%#v", node)
	}
	script := compactKbuildRecipeScriptContentForTest(t, plan.Recipes[node.Recipe])
	for _, fragment := range []string{
		"rm -f vmlinux.a",
		"ar cDPrST vmlinux.a built-in.a lib/lib.a arch/x86/lib/lib.a",
		"ar mPiT $(ar t vmlinux.a | sed -n 1p) vmlinux.a $(ar t vmlinux.a | grep -F init/main.o)",
	} {
		if !strings.Contains(script, fragment) {
			t.Fatalf("root compound archive script=%q, want fragment %q", script, fragment)
		}
	}
	for _, nonCanonical := range []string{"./vmlinux.a", "./built-in.a", "./lib/lib.a", "./arch/x86/lib/lib.a"} {
		if strings.Contains(script, nonCanonical) {
			t.Fatalf("root compound archive script contains non-canonical path %q: %q", nonCanonical, script)
		}
	}
}

func TestTypedWorkingArgumentCanonicalizesRootMarkerPrefixes(t *testing.T) {
	logicalPaths := map[string]bool{"drivers/example.o": true}
	for _, test := range []struct {
		name, value, objectRoot, want string
	}{
		{name: "prep path", value: "${tree:prep}/include/generated", objectRoot: ".", want: "include/generated"},
		{name: "work path in option", value: "-I${work:root}/include", objectRoot: ".", want: "-Iinclude"},
		{name: "exact root", value: "${tree:prep}", objectRoot: ".", want: "."},
		{name: "nested root", value: "${tree:prep}/include/generated", objectRoot: "../..", want: "../../include/generated"},
		{name: "logical root path", value: "drivers/example.o", objectRoot: ".", want: "drivers/example.o"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := compactKbuildTypedWorkingArgument(test.value, test.objectRoot, logicalPaths); got != test.want {
				t.Fatalf("typed working argument=%q, want %q", got, test.want)
			}
		})
	}
}

func TestCompoundProgramDiscoveryRejectsCommandSubstitutionInProgramHead(t *testing.T) {
	for _, recipe := range []string{
		"$(choose-tool) argument",
		"prefix$(choose-tool)suffix argument",
	} {
		if _, err := compactKbuildCompoundProgramCommands(recipe); err == nil || !strings.Contains(err.Error(), "dynamic program") {
			t.Fatalf("compound program %q error=%v, want dynamic-program rejection", recipe, err)
		}
	}
}

func TestCompoundProgramDiscoveryTreatsArithmeticExpansionAsArgumentData(t *testing.T) {
	recipe := "outer pre$((1 + (2 * 3)))post argument; next done"
	commands, err := compactKbuildCompoundProgramCommands(recipe)
	if err != nil {
		t.Fatal(err)
	}
	programs := make([]string, len(commands))
	for index, command := range commands {
		programs[index] = command.program
	}
	if want := []string{"outer", "next"}; !slices.Equal(programs, want) {
		t.Fatalf("arithmetic expansion programs=%q, want %q", programs, want)
	}
}

func TestEvaluatedKbuildDiagnosticRecipeIsOrderingOnly(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "subcmd", "tools/lib/subcmd/Makefile", "tools/lib/subcmd", `
QUIET_INSTALL = @printf '  INSTALL %s\n' $(1);
install_headers: installed/one.h installed/two.h
	$(call QUIET_INSTALL,libsubcmd_headers)
`, nil)
	match := compactKbuildRuleMatch{
		profile:      profile,
		rule:         profile.Rules[0],
		lookupTarget: "tools/lib/subcmd/install_headers",
		explicit:     true,
		resolved:     true,
	}
	action, directorySetup, err := evaluatedKbuildDirectRecipeEffects("tools/lib/subcmd/install_headers", match)
	if err != nil {
		t.Fatal(err)
	}
	if action || directorySetup {
		t.Fatalf("diagnostic recipe effects = action:%t directory:%t, want ordering only", action, directorySetup)
	}
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
	orderingOnly, err := metadata.compactKbuildTargetIsOrderingOnlyInProfile(profile, "tools/lib/subcmd/install_headers")
	if err != nil {
		t.Fatal(err)
	}
	if !orderingOnly {
		t.Fatal("diagnostic target was classified as an artifact producer")
	}
}

func TestEvaluatedKbuildDiagnosticAndMkdirRecipeIsOrderingOnly(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "resolve-btfids", "tools/bpf/resolve_btfids/Makefile", "tools/bpf/resolve_btfids", `
msg = @printf '  %-8s %s%s\n' "$(1)" "$(notdir $(2))" "$(if $(3), $(3))";
Q = @
libsubcmd:
	$(call msg,MKDIR,,$@)
	$(Q)mkdir -p $(@)
`, nil)
	target := "tools/bpf/resolve_btfids/libsubcmd"
	directMatch := compactKbuildRuleMatch{
		profile: profile, rule: profile.Rules[0], lookupTarget: target, explicit: true, resolved: true,
	}
	action, directorySetup, err := evaluatedKbuildDirectRecipeEffects(target, directMatch)
	if err != nil {
		t.Fatal(err)
	}
	if action || !directorySetup {
		stem, normal, orderOnly, contextErr := compactKbuildRuleEvaluationContext(target, directMatch, nil)
		if contextErr != nil {
			t.Fatal(contextErr)
		}
		injected, contextErr := compactKbuildSourceScriptInjectionsForTarget(profile, target, stem, normal, orderOnly)
		if contextErr != nil {
			t.Fatal(contextErr)
		}
		automatic, contextErr := compactKbuildRuleRootedAutomaticEvaluationContext(target, directMatch, nil, injected)
		if contextErr != nil {
			t.Fatal(contextErr)
		}
		evaluated := []string{}
		for _, raw := range directMatch.rule.Recipe {
			line, evaluateErr := evaluateCompactKbuildTextWithAutomaticTarget(
				profile, target, automatic.target, automatic.stem,
				automatic.normal, automatic.order, injected, raw, true,
			)
			if evaluateErr != nil {
				t.Fatal(evaluateErr)
			}
			evaluated = append(evaluated, compactKbuildDirectRecipeText(profile, line))
		}
		t.Fatalf("diagnostic mkdir effects = action:%t directory:%t, want ordering-only directory setup; evaluated=%q", action, directorySetup, evaluated)
	}
	match, matched, err := (&CompactMetadata{}).compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatalf("diagnostic mkdir target resolved as artifact-producing rule %#v", match)
	}
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
	orderingOnly, err := metadata.compactKbuildTargetIsOrderingOnlyInProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !orderingOnly {
		t.Fatal("diagnostic mkdir target was classified as an artifact producer")
	}
}

func TestEvaluatedKbuildCommandTemplateDiagnosticAndMkdirIsOrderingOnly(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "resolve-btfids", "tools/bpf/resolve_btfids/Makefile", "tools/bpf/resolve_btfids", `
msg = printf '  %-8s %s%s\n' "MKDIR" "" " $@";
cmd_setup = $(msg) mkdir -p $@
cmd_copy = cat $< > $@
libsubcmd: FORCE
	$(call cmd,setup)
%: %_shipped
	$(call cmd,copy)
`, nil)
	target := "tools/bpf/resolve_btfids/libsubcmd"
	profile = compactKbuildProfileWithSourcesForTest(t, profile, target+"_shipped")
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
	if match, matched, err := metadata.compactKbuildRuleForProfile(profile, target); err != nil {
		t.Fatal(err)
	} else if matched {
		t.Fatalf("diagnostic mkdir command template resolved as artifact rule %#v", match)
	}
	orderingOnly, err := metadata.compactKbuildTargetIsOrderingOnlyInProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !orderingOnly {
		t.Fatal("explicit diagnostic mkdir command template fell through to viable implicit shipped rule")
	}
}

func TestEvaluatedKbuildSourceScriptTemplateIsArtifactAction(t *testing.T) {
	const directory = "arch/x86/kernel/cpu"
	profile := mustCompactKbuildProfileForTest(t, "cpu", "arch/x86/kernel/cpu/Makefile", directory, `
CONFIG_SHELL := sh
cmd_mkcapflags = $(CONFIG_SHELL) $(src)/mkcapflags.sh $@ $^
cpufeature = $(src)/../../include/asm/cpufeatures.h
vmxfeature = $(src)/../../include/asm/vmxfeatures.h
$(obj)/capflags.c: $(cpufeature) $(vmxfeature) $(src)/mkcapflags.sh FORCE
	$(call if_changed,mkcapflags)
targets += capflags.c

cmd_copy = cat $< > $@
$(obj)/%: $(src)/%_shipped
	$(call cmd,copy)
`, map[string]string{"obj": directory, "src": directory})
	profile = compactKbuildProfileWithSourcesForTest(
		t, profile,
		"arch/x86/include/asm/cpufeatures.h",
		"arch/x86/include/asm/vmxfeatures.h",
		directory+"/mkcapflags.sh",
	)
	target := directory + "/capflags.c"
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
	match, matched, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !matched || !match.explicit || !slices.Equal(match.commandSequence(), []string{"mkcapflags"}) {
		t.Fatalf("source-script generated target match = (%#v,%t), want explicit mkcapflags action instead of shipped fallback", match, matched)
	}
}

func TestEvaluatedKbuildExplicitGeneratedRuleBeatsShippedFallbackWhenCommandIsDefinedLater(t *testing.T) {
	const directory = "lib"
	profile := mustCompactKbuildProfileForTest(t, "build:lib", "scripts/Makefile.build", directory, `
quiet_cmd_copy = COPY $@
cmd_copy = cat $< > $@
$(obj)/%: $(src)/%_shipped
	$(call cmd,copy)

$(obj)/oid_registry_data.c: $(srctree)/include/linux/oid_registry.h $(src)/build_OID_registry
	$(call cmd,build_OID_registry)

quiet_cmd_build_OID_registry = GEN $@
cmd_build_OID_registry = perl $(src)/build_OID_registry $< $@
`, map[string]string{
		"obj":     directory,
		"src":     directory,
		"srctree": "__LINUX_BZL_SOURCE_TREE__",
	})
	profile = compactKbuildProfileWithSourcesForTest(
		t, profile, "include/linux/oid_registry.h", directory+"/build_OID_registry",
	)
	root := profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	mustWriteSource(t, root, directory+"/build_OID_registry", `#!/usr/bin/perl -w
use strict;
open(my $output, '>', $ARGV[1]) or die $!;
print {$output} "generated\n";
`)
	if variable, ok := profile.evaluator.template.lookupVariable("cmd_build_OID_registry"); !ok || variable.value == "" {
		t.Fatalf("late-defined OID registry command was not captured: (%#v, %t)", variable, ok)
	}
	target := directory + "/oid_registry_data.c"
	perlRole := compactKbuildScriptAppletRolePrefix + "perl"
	metadata := &CompactMetadata{
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
		actionRoles: testTargetActionRoles(perlRole),
	}
	match, matched, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !matched || !match.explicit || !slices.Equal(match.commandSequence(), []string{"build_OID_registry"}) {
		t.Fatalf("OID registry match = (%#v,%t), want explicit late-defined generator instead of shipped fallback", match, matched)
	}

	plan := &ActionPlan{
		Toolsets: map[string]string{
			"target": "sha256-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		Recipes: map[string]ActionRecipe{},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("OID registry source-script producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("OID registry source-script tool=%q/%q, want %q", node.Tool, recipe.Tool, compactKbuildScriptRunnerRole)
	}
	wantPrefix := []string{
		"-interpreter", "${tool:" + perlRole + "}",
		"-interpreter_arg", "-w",
		"-multicall", "${tool:" + compactKbuildScriptRuntimeRole + "}",
	}
	if len(recipe.Arguments) < len(wantPrefix) || !slices.Equal(recipe.Arguments[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("OID registry source-script arguments=%q, want interpreter prefix %q", recipe.Arguments, wantPrefix)
	}
	if slices.Contains(recipe.Arguments, "perl") {
		t.Fatalf("OID registry source-script arguments inject perl basename dispatch: %q", recipe.Arguments)
	}
	if !slices.Contains(node.AuxiliaryTools, perlRole) || !slices.Contains(recipe.AuxiliaryTools, perlRole) {
		t.Fatalf("OID registry interpreter role missing from tool closure: node=%q recipe=%q", node.AuxiliaryTools, recipe.AuxiliaryTools)
	}
	scriptEdge := false
	for _, source := range node.Sources {
		if source.Role == "script" {
			scriptEdge = true
		}
	}
	if !scriptEdge {
		t.Fatalf("OID registry action lacks immutable source-script edge: %#v", node)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("OID registry canonical plan validation failed: %v", err)
	}
}

func TestKbuildSourceScriptSavecmdWrapperSelectsLeafAction(t *testing.T) {
	const directory = "arch/x86/kernel/cpu"
	target := directory + "/capflags.c"
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
CONFIG_SHELL := sh
cmd = $(if $(cmd_$(1)),set -e; $(cmd_$(1)),:)
make-cmd = $(cmd_$(1))
dot-target = $(dir $@).$(notdir $@)
cmd_and_savecmd = $(cmd); printf '%s\n' 'savedcmd_$@ := $(make-cmd)' > $(dot-target).cmd
if-changed-cond = 1
if_changed = $(if $(if-changed-cond),$(cmd_and_savecmd),@:)

cmd_mkcapflags = $(CONFIG_SHELL) $(src)/mkcapflags.sh $@ $^
cpufeature = $(src)/../../include/asm/cpufeatures.h
vmxfeature = $(src)/../../include/asm/vmxfeatures.h
$(obj)/capflags.c: $(cpufeature) $(vmxfeature) $(src)/mkcapflags.sh FORCE
	$(call if_changed,mkcapflags)
`, map[string]string{"obj": directory, "src": directory})
	profile = compactKbuildProfileWithSourcesForTest(
		t, profile,
		"arch/x86/include/asm/cpufeatures.h",
		"arch/x86/include/asm/vmxfeatures.h",
		directory+"/mkcapflags.sh",
	)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !slices.Equal(match.commandSequence(), []string{"mkcapflags"}) {
		t.Fatalf("savecmd wrapper selected match=%#v found=%t, want mkcapflags leaf", match, found)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
		metadata: metadata,
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("host", "host", "sdk")
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("node count=%d, want one atomic source-script wrapper: %#v", got, plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing producer %q", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Stage != "host" || node.Tool != compactKbuildScriptRunnerRole ||
		recipe.Tool != compactKbuildScriptRunnerRole || node.Outputs[0].Path != target {
		t.Fatalf("atomic source-script producer=%#v recipe=%#v", node, recipe)
	}
	recipeText := compactKbuildRecipeScriptContentForTest(t, recipe)
	if !strings.Contains(recipeText, "${tree:kernel}/"+directory+"/mkcapflags.sh") ||
		!strings.Contains(recipeText, target) {
		t.Fatalf("selected source-script wrapper=%q, want immutable script execution for %q", recipeText, target)
	}
	for _, want := range []string{"savedcmd_", ".capflags.c.cmd"} {
		if !strings.Contains(recipeText, want) {
			t.Errorf("selected source-script wrapper omits %q: %q", want, recipe.Arguments)
		}
	}
	for _, unwanted := range []string{"$(1)", "cmd_and_savecmd"} {
		if strings.Contains(recipeText, unwanted) {
			t.Errorf("selected source-script wrapper retains unevaluated Make text %q: %q", unwanted, recipe.Arguments)
		}
	}
	if len(node.Outputs) != 2 || node.Outputs[1].Path != directory+"/.capflags.c.cmd" {
		t.Fatalf("savecmd outputs=%#v, want target and semantic command state", node.Outputs)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("write atomic source-script wrapper plan: %v", err)
	}
}

func TestEvaluatedKbuildInvocationLocalTargetConcatenatesWithOutputOnce(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "tools", "tools/Makefile", "tools", `
OUTPUT = $(obj)/
objtool:
	mkdir -p $(OUTPUT)$@
`, nil)
	target := "tools/objtool"
	match := compactKbuildRuleMatch{
		profile: profile, rule: profile.Rules[0], lookupTarget: target, explicit: true, resolved: true,
	}
	action, directorySetup, err := evaluatedKbuildDirectRecipeEffects(target, match)
	if err != nil {
		t.Fatal(err)
	}
	if action || !directorySetup {
		t.Fatalf("invocation-local output setup effects = action:%t directory:%t", action, directorySetup)
	}
	stem, normal, orderOnly, err := compactKbuildRuleEvaluationContext(target, match, nil)
	if err != nil {
		t.Fatal(err)
	}
	injected, err := compactKbuildSourceScriptInjectionsForTarget(profile, target, stem, normal, orderOnly)
	if err != nil {
		t.Fatal(err)
	}
	automatic, err := compactKbuildRuleRootedAutomaticEvaluationContext(target, match, nil, injected)
	if err != nil {
		t.Fatal(err)
	}
	evaluated, err := evaluateCompactKbuildTextWithAutomaticTarget(
		profile, target, automatic.target, automatic.stem,
		automatic.normal, automatic.order, injected, match.rule.Recipe[0], true,
	)
	if err != nil {
		t.Fatal(err)
	}
	evaluated = compactKbuildDirectRecipeText(profile, evaluated)
	if strings.Count(evaluated, "${tree:prep}") != 1 || evaluated != "mkdir -p ${tree:prep}/tools/objtool" {
		t.Fatalf("evaluated invocation-local mkdir = %q, want one rooted output path", evaluated)
	}
}

func TestKbuildRootedAutomaticWordPreservesMakeSpelling(t *testing.T) {
	marker := "__LINUX_BZL_OBJECT_TREE__"
	for _, test := range []struct {
		name, word, graphPath, want string
	}{
		{name: "invocation local", word: "objtool", graphPath: "tools/objtool", want: "objtool"},
		{name: "graph rooted", word: "tools/objtool", graphPath: "tools/objtool", want: marker + "/tools/objtool"},
		{name: "already rooted", word: marker + "/tools/objtool", graphPath: "tools/objtool", want: marker + "/tools/objtool"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := compactKbuildRootedAutomaticWord(test.word, test.graphPath, marker); got != test.want {
				t.Fatalf("rooted automatic word = %q, want %q", got, test.want)
			}
		})
	}
}

func compactGenericRecipePlanForTest() *ActionPlan {
	return &ActionPlan{
		Recipes: map[string]ActionRecipe{},
		Nodes: []ActionPlanNode{{
			ID: strings.Repeat("b", 64), Stage: "host", Kind: "generate", Tool: "cc", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: "host", Path: "tools/filter"}},
		}},
	}
}

func buildCompactKbuildTargetForTest(metadata *CompactMetadata, plan *ActionPlan, target string) error {
	if metadata == nil || len(metadata.Config.KbuildProfiles) != 1 {
		return fmt.Errorf("generic Kbuild target test requires one exact profile")
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(metadata.Config.KbuildProfiles[0])
	_, err := builder.build(target)
	return err
}

func TestGenericKbuildRulePreservesOrderOnlyInputRole(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:order-only", "scripts/Makefile.build", "", `
all: input.txt | generated.order
	cp input.txt $@
generated.order: order.in
	cp $< $@
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.txt", "order.in")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, "all"); err != nil {
		t.Fatal(err)
	}
	orderProducer, _, ok := planProducerByOutput(plan, "objects", "generated.order")
	if !ok {
		t.Fatalf("generated.order has no native producer: %#v", plan.Nodes)
	}
	allProducer, _, ok := planProducerByOutput(plan, "objects", "all")
	if !ok {
		t.Fatalf("all has no native producer: %#v", plan.Nodes)
	}
	all, ok := compactKbuildPlanNode(plan, allProducer)
	if !ok {
		t.Fatalf("all producer %q is absent", allProducer)
	}
	if !slices.Contains(all.Inputs, ActionPlanNodeEdge{
		Role: "order-only", ProducerID: orderProducer, Slot: 0,
	}) {
		t.Fatalf("all inputs = %#v, want typed order-only edge from %s", all.Inputs, orderProducer)
	}
}

func TestLiteralFilechkKeepsWorkingClosureRoleAheadOfOrderOnly(t *testing.T) {
	producer := ActionPlanNode{
		ID:      strings.Repeat("a", 64),
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: "generated/baseline.h"}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}, Nodes: []ActionPlanNode{producer}}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		forOutput("target", "objects", "vmlinux")
	if _, err := builder.appendCompactKbuildLiteralFilechk(
		"generated/result.h",
		compactKbuildRuleMatch{},
		[]compactKbuildRuleInput{{
			path: "generated/baseline.h", producer: producer.ID,
			workingOnly: true, orderOnly: true,
		}},
		[]string{"#define RESULT 1"},
	); err != nil {
		t.Fatal(err)
	}
	if got, want := plan.Nodes[1].Inputs, []ActionPlanNodeEdge{{
		Role: compactKbuildWorkingClosureInputRole, ProducerID: producer.ID,
	}}; !slices.Equal(got, want) {
		t.Fatalf("literal filechk inputs = %#v, want working-closure precedence %#v", got, want)
	}
}

func TestExactGeneratedContentReachesFinalLoweringAndConfigAnalysis(t *testing.T) {
	const target = "include/generated/measured.h"
	const exact = "#if defined(CONFIG_MEASURED_FEATURE)\n#define MEASURED 1\n#endif\n"
	root := t.TempDir()
	mustWriteSource(t, root, "kernel/time/timeconst.bc", "scale=250\n")
	profile := mustCompactKbuildProfileForTest(t, "build:measured-generator", "Makefile", "", `
all: `+target+`
`+target+`: kernel/time/timeconst.bc FORCE
	{ echo 250 | bc -q ${tree:kernel}/kernel/time/timeconst.bc; } > $@
.PHONY: FORCE
FORCE:
`, nil)
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__": root,
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{{
				Profile: profile.Name, Target: target, MakeTarget: target,
				Lifecycle: "target", Scope: "target", Stage: "target",
				ExactGeneratedContent: exact, ExactGeneratedContentSet: true,
			}},
		},
		configFragment:       map[string]string{"CONFIG_MEASURED_FEATURE": "y"},
		actionRoles:          testConfiguredScopedActionRoles,
		selectedProductsOnly: true,
	}
	plan, dependencies, err := metadata.ActionPlanWithConfigDependencies(
		bootstrapTestIdentity, bootstrapTestIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	producer, _, ok := planProducerByOutput(plan, "objects", target)
	if !ok {
		t.Fatalf("measured header has no final producer: %#v", plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("measured header producer %q is absent", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	wantArguments := []string{
		"-content_base64", base64.StdEncoding.EncodeToString([]byte(exact)),
		"-out", "${output:00000000}",
	}
	if node.Tool != "actionfile" || recipe.Tool != "actionfile" ||
		!slices.Equal(recipe.Arguments, wantArguments) {
		t.Fatalf("measured header node=%#v recipe=%#v, want exact actionfile", node, recipe)
	}
	if len(node.Sources) != 0 || len(node.Inputs) != 0 || len(node.Trees) != 0 ||
		len(recipe.Sources) != 0 || len(recipe.Inputs) != 0 || len(recipe.Trees) != 0 ||
		len(recipe.Environment) != 0 {
		t.Fatalf("measured literal retained originating config/source state: node=%#v recipe=%#v", node, recipe)
	}
	dependency, ok := dependencies[node.ID]
	if !ok {
		t.Fatalf("measured header has no config dependency annotation: %#v", dependencies)
	}
	if dependency.Opaque || len(dependency.Symbols) != 0 || len(dependency.ObjectPaths) != 0 {
		t.Fatalf("measured header producer config dependency = %#v, want config-free literal", dependency)
	}

	compilePlan, compileNode := configDependencyCompilePlanForTest(t, map[string]string{
		"drivers/example/driver.c": "#include <measured.h>\nCONFIG_DRIVER\n",
	}, []string{
		"-I${tree:prep}/include/generated", "-c", "drivers/example/driver.c",
	}, nil)
	configDependencyStageGeneratedHeaderForTest(
		t, compilePlan, &compileNode, "measured-header", target, recipe,
	)
	consumerDependency, err := AnalyzeActionPlanNodeConfigDependencies(compilePlan, compileNode)
	if err != nil {
		t.Fatal(err)
	}
	if consumerDependency.Opaque ||
		!slices.Contains(consumerDependency.Symbols, "CONFIG_DRIVER") ||
		!slices.Contains(consumerDependency.Symbols, "CONFIG_MEASURED_FEATURE") {
		t.Fatalf("consumer of measured header config dependency = %#v, want exact generated text", consumerDependency)
	}
}

func TestExactGeneratedContentWithGeneratedPrerequisiteKeepsConservativeFinalAction(t *testing.T) {
	const target = "include/generated/measured.h"
	profile := mustCompactKbuildProfileForTest(t, "build:measured-generator", "Makefile", "", `
all: `+target+`
generated/value: FORCE
	printf '%s\n' dynamic > $@
`+target+`: generated/value FORCE
	cp $< $@
.PHONY: FORCE
FORCE:
`, nil)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{{
				Profile: profile.Name, Target: target, MakeTarget: target,
				Lifecycle: "target", Scope: "target", Stage: "target",
				ExactGeneratedContent: "dynamic\n", ExactGeneratedContentSet: true,
			}},
		},
		configFragment:       map[string]string{},
		actionRoles:          testConfiguredScopedActionRoles,
		selectedProductsOnly: true,
	}
	plan, err := metadata.ActionPlan(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	producer, _, ok := planProducerByOutput(plan, "objects", target)
	if !ok {
		t.Fatalf("measured header has no final producer: %#v", plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("measured header producer %q is absent", producer)
	}
	if node.Tool == "actionfile" || plan.Recipes[node.Recipe].Tool == "actionfile" {
		t.Fatalf("producer-backed exact candidate was replaced by a literal: node=%#v recipe=%#v", node, plan.Recipes[node.Recipe])
	}
}

func TestExactGeneratedContentWithClosureOnlyConfigWriterKeepsConservativeFinalAction(t *testing.T) {
	const (
		target       = "include/generated/measured.h"
		source       = "kernel/time/timeconst.bc"
		configInput  = "kernel.release"
		configOutput = "include/config/kernel.release"
	)
	prepProfile := CompactKbuildProfile{
		Name: "prep-config", Path: "Makefile", EntryTargets: []string{configOutput},
	}
	profile := CompactKbuildProfile{
		Name: "build:measured-generator", Path: "Makefile", EntryTargets: []string{target},
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	consumerKey := compactKbuildSelectionKey{
		profile: profile.Name, target: target, stage: "target",
	}
	prepKey := compactKbuildSelectionKey{
		profile: prepProfile.Name, target: configOutput, stage: "prep",
	}
	graph, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{prepProfile, profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: prepProfile.Name, Target: configOutput, MakeTarget: configOutput, Lifecycle: "prep", Scope: "target", Stage: "prep"},
			{Profile: profile.Name, Target: target, MakeTarget: target, Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{}
	configSourceIDs := map[string]string{}
	for _, projection := range resolvedConfigProjections() {
		sourceID, sourceErr := ensureActionPlanSource(plan, "config", projection.input)
		if sourceErr != nil {
			t.Fatal(sourceErr)
		}
		configSourceIDs[projection.input] = sourceID
	}
	projection := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "prep", Kind: "copy", Tool: "actionfile", Product: "sdk",
		Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: configSourceIDs[configInput]}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: configOutput}},
	}
	plan.Nodes = []ActionPlanNode{projection}
	sourceID, err := ensureActionPlanSource(plan, "kernel", source)
	if err != nil {
		t.Fatal(err)
	}
	direct := []compactKbuildRuleInput{{path: source, sourceID: sourceID}}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
		withSelectionGraph(graph).
		forSelection(consumerKey, profile).
		forOutput("target", "objects", "sdk")

	closed, err := builder.compactKbuildExactGeneratedContentFrontier(target, profile, direct)
	if err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("immutable direct source and config projections did not form a closed exact-content frontier")
	}
	if err := graph.recordMaterializedProducer(prepKey, projection.ID); err != nil {
		t.Fatal(err)
	}
	closure, err := builder.compactKbuildWorkingTreeClosureInputs(target, profile, direct)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(closure, func(input compactKbuildRuleInput) bool {
		return input.path == configOutput && input.producer == projection.ID && input.sourceID == "" && input.workingOnly
	}) {
		t.Fatalf("working-tree closure = %#v, want materialized config writer %q", closure, projection.ID)
	}
	closed, err = builder.compactKbuildExactGeneratedContentFrontier(target, profile, direct)
	if err != nil {
		t.Fatal(err)
	}
	if closed {
		t.Fatal("closure-only materialized config writer was accepted as an immutable exact-content frontier")
	}
}

func TestDynamicGeneratedContentKeepsConservativeFinalAction(t *testing.T) {
	const target = "include/generated/dynamic.h"
	profile := mustCompactKbuildProfileForTest(t, "build:dynamic-generator", "Makefile", "", `
DYNAMIC = config-sensitive
export DYNAMIC
all: `+target+`
`+target+`: FORCE
	{ printf '%s\n' "$$DYNAMIC"; } > $@
.PHONY: FORCE
FORCE:
`, nil)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildSelections: []CompactKbuildSelection{{
				Profile: profile.Name, Target: target, MakeTarget: target,
				Lifecycle: "target", Scope: "target", Stage: "target",
			}},
		},
		configFragment:       map[string]string{"CONFIG_DYNAMIC_FEATURE": "y"},
		actionRoles:          testConfiguredScopedActionRoles,
		selectedProductsOnly: true,
	}
	plan, dependencies, err := metadata.ActionPlanWithConfigDependencies(
		bootstrapTestIdentity, bootstrapTestIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	producer, _, ok := planProducerByOutput(plan, "objects", target)
	if !ok {
		t.Fatalf("dynamic header has no final producer: %#v", plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("dynamic header producer %q is absent", producer)
	}
	if node.Tool == "actionfile" || plan.Recipes[node.Recipe].Tool == "actionfile" {
		t.Fatalf("dynamic generator was replaced by an unproven literal: node=%#v recipe=%#v", node, plan.Recipes[node.Recipe])
	}
	dependency := dependencies[node.ID]
	if !dependency.Opaque {
		t.Fatalf("dynamic generator config dependency = %#v, want conservative opaque fallback", dependency)
	}
}

func TestGenericKbuildRecipeLowersGeneratedToolPipelineAndRedirect(t *testing.T) {
	metadata, target := compactGenericRecipeMetadataForTest(
		t, `tools/filter $< | $(NM) --format=posix > $@`, ":", "generated/result.h",
	)
	plan := compactGenericRecipePlanForTest()
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 2; got != want {
		t.Fatalf("node count=%d, want %d: %#v", got, want, plan.Nodes)
	}
	result := plan.Nodes[1]
	resultRecipe := plan.Recipes[result.Recipe]
	if result.Tool != compactKbuildScriptRunnerRole || result.Outputs[0].Path != target {
		t.Fatalf("atomic pipeline=%#v recipe=%#v", result, resultRecipe)
	}
	if resultRecipe.Stdin != "" || resultRecipe.Stdout != "" || resultRecipe.ExecutionDirectory != "" {
		t.Fatalf("atomic pipeline retains split stream/cwd state: %#v", resultRecipe)
	}
	if len(resultRecipe.ExecutableInputs) != 1 || !slices.Contains(resultRecipe.AuxiliaryTools, "nm") || !slices.Contains(resultRecipe.AuxiliaryTools, compactKbuildScriptRuntimeRole) {
		t.Fatalf("atomic pipeline bindings=%#v", resultRecipe)
	}
	script := compactKbuildRecipeScriptContentForTest(t, resultRecipe)
	if !strings.Contains(script, "tools/filter ${tree:kernel}/input.txt | nm --format=posix > "+target) {
		t.Fatalf("atomic pipeline script=%q", script)
	}
	if strings.Contains(script, ".linux-bzl-intermediate") {
		t.Fatalf("atomic pipeline serialized an intermediate stream: %q", script)
	}
}

func TestGenericKbuildRecipeMarksPrehostProgramExecutable(t *testing.T) {
	metadata, target := compactGenericRecipeMetadataForTest(
		t, `tools/filter $< | $(NM) --format=posix > $@`, ":", "generated/result.h",
	)
	plan := compactGenericRecipePlanForTest()
	plan.Nodes[0].Stage = "prehost"
	plan.Nodes[0].Outputs[0].Tree = "prehost"
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	recipe := plan.Recipes[plan.Nodes[1].Recipe]
	if len(recipe.ExecutableInputs) != 1 {
		t.Fatalf("prehost generated program executable inputs = %#v, want one", recipe.ExecutableInputs)
	}
}

func TestGenericKbuildRecipeStagesSourceProgramBeforeHost(t *testing.T) {
	for _, stage := range []string{"prehost", "bootstrap"} {
		t.Run(stage, func(t *testing.T) {
			metadata, target := compactGenericRecipeMetadataForTest(
				t, `tools/filter $< > $@`, ":", "generated/result.h",
			)
			profile := compactKbuildProfileWithSourcesForTest(
				t, metadata.Config.KbuildProfiles[0], "input.txt", "tools/filter",
			)
			metadata.Config.KbuildProfiles[0] = profile
			plan := &ActionPlan{
				Toolsets: map[string]string{
					"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity,
				},
				Recipes: map[string]ActionRecipe{},
			}
			builder := newCompactKbuildRulePlanBuilder(metadata, plan).
				forOutput(stage, stage, "sdk").
				forProfile(profile)
			if _, err := builder.build(target); err != nil {
				t.Fatal(err)
			}
			program, _, ok := planProducerByOutput(plan, stage, "tools/filter")
			if !ok {
				t.Fatalf("source program has no %s-stage executable copy: %#v", stage, plan.Nodes)
			}
			programNode, ok := compactKbuildPlanNode(plan, program)
			if !ok || programNode.Stage != stage || programNode.Outputs[0].Tree != stage {
				t.Fatalf("source program node = %#v, want %s stage/tree", programNode, stage)
			}
			consumer, _, ok := planProducerByOutput(plan, stage, target)
			if !ok {
				t.Fatalf("target has no %s-stage producer: %#v", stage, plan.Nodes)
			}
			consumerNode, _ := compactKbuildPlanNode(plan, consumer)
			consumerRecipe := plan.Recipes[consumerNode.Recipe]
			if !slices.ContainsFunc(consumerNode.Inputs, func(input ActionPlanNodeEdge) bool {
				return input.ProducerID == program
			}) || len(consumerRecipe.ExecutableInputs) != 1 {
				t.Fatalf("source-program consumer = %#v recipe=%#v, want executable dependency %s", consumerNode, consumerRecipe, program)
			}
		})
	}
}

func TestGenericKbuildRecipeParseErrorPipelineStillMaterializesProgramHeads(t *testing.T) {
	metadata, target := compactGenericRecipeMetadataForTest(
		t, `tools/filter $< | $(NM) > $@ || true`, ":", "generated/result.h",
	)
	plan := compactGenericRecipePlanForTest()
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 2; got != want {
		t.Fatalf("node count=%d, want generated tool + compound action: %#v", got, plan.Nodes)
	}
	recipe := plan.Recipes[plan.Nodes[1].Recipe]
	if plan.Nodes[1].Tool != compactKbuildScriptRunnerRole || len(recipe.ExecutableInputs) != 1 {
		t.Fatalf("parse-error compound program bindings=%#v node=%#v", recipe, plan.Nodes[1])
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if !strings.Contains(script, `tools/filter ${tree:kernel}/input.txt | nm > `+target+` || true`) {
		t.Fatalf("parse-error compound script=%q", script)
	}
}

func TestGenericKbuildRecipeParseErrorPipelineRejectsAmbientProgramPath(t *testing.T) {
	metadata, target := compactGenericRecipeMetadataForTest(
		t, `/ambient/filter $< | $(NM) > $@ || true`, ":", "generated/result.h",
	)
	err := buildCompactKbuildTargetForTest(metadata, compactGenericRecipePlanForTest(), target)
	if err == nil || !strings.Contains(err.Error(), `non-hermetic program "/ambient/filter"`) {
		t.Fatalf("ambient compound program error=%v", err)
	}
}

func TestGenericKbuildCompoundProjectsAbsoluteRuntimeCommandHeadOnly(t *testing.T) {
	metadata, target := compactGenericRecipeMetadataForTest(
		t, `printf '%s' '/bin/false' | /bin/false || true > $@`, ":", "generated/result.h",
	)
	plan := compactGenericRecipePlanForTest()
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	recipe := plan.Recipes[plan.Nodes[len(plan.Nodes)-1].Recipe]
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if !strings.Contains(script, `printf '%s' '/bin/false' | false || true > `+target) {
		t.Fatalf("absolute runtime command-head projection mutated data or missed program: %q", script)
	}
}

func TestGenericKbuildCompoundPreservesLiteralTreeMarkerPayloads(t *testing.T) {
	metadata, target := compactGenericRecipeMetadataForTest(
		t, `printf '%s\n' '__LINUX_BZL_OBJECT_TREE__' '__LINUX_BZL_MAKE__' '$${tree:prep}' | tools/filter > $@`, ":", "generated/result.h",
	)
	plan := compactGenericRecipePlanForTest()
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	recipe := plan.Recipes[plan.Nodes[len(plan.Nodes)-1].Recipe]
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	for _, literal := range []string{`'__LINUX_BZL_OBJECT_TREE__'`, `'__LINUX_BZL_MAKE__'`, `'${tree:prep}'`} {
		if !strings.Contains(script, literal) {
			t.Fatalf("compound script mutated literal marker %q: %q", literal, script)
		}
	}
	if !slices.Contains(recipe.Arguments, "-literal_tree_offset") || slices.ContainsFunc(recipe.Arguments, func(argument string) bool {
		return strings.HasPrefix(argument, "prep=")
	}) {
		t.Fatalf("literal marker provenance bindings=%#v", recipe.Arguments)
	}
	if len(recipe.CommandReplays) != 0 {
		t.Fatalf("source-authored recursive Make lookalike created replay capability: %#v", recipe.CommandReplays)
	}
}

func TestGenericKbuildCompoundSeparatesConstructedMakeLiteralFromRecursiveMake(t *testing.T) {
	const target = "generated/result.h"
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
make_marker_prefix := __LINUX_BZL_
cmd_transform = printf '%s\n' '$(make_marker_prefix)MAKE__' > $@; $(MAKE) child.o
`+target+`: FORCE
	$(call if_changed,transform)
`, map[string]string{"MAKE": CompactKbuildRecursiveMakeProvenanceToken})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	profile.TargetInvocationDependencies = []CompactKbuildInvocationDependency{{
		Target: target, Profile: "build:child", Goals: []string{"child.o"},
		ReplayArguments: []string{"child.o"},
	}}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	childRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	childNode := ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "actionfile", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "child.o"}},
	}
	if _, err := appendActionPlanNode(plan, childNode, childRecipe); err != nil {
		t.Fatal(err)
	}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	recipe := plan.Recipes[plan.Nodes[len(plan.Nodes)-1].Recipe]
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if strings.Count(script, compactKbuildRecursiveMakeMarker) != 1 ||
		!strings.Contains(script, "'"+compactKbuildRecursiveMakeMarker+"'") ||
		!strings.Contains(script, "make child.o") ||
		strings.Contains(script, CompactKbuildRecursiveMakeProvenanceToken) {
		t.Fatalf("constructed literal and recursive Make lost provenance: %q", script)
	}
	if len(recipe.CommandReplays) != 1 ||
		!slices.Equal(recipe.CommandReplays[0].Invocations[0].Arguments, []string{"child.o"}) {
		t.Fatalf("recursive Make replay = %#v, want exact child.o invocation", recipe.CommandReplays)
	}
}

func TestGenericKbuildCompoundRejectsPrivateProvenanceByteBeforeScriptEncoding(t *testing.T) {
	const target = "generated/result.h"
	for _, boundary := range []struct {
		name  string
		value string
	}{{name: "opening", value: "\x05"}, {name: "closing", value: "\x06"}} {
		t.Run(boundary.name, func(t *testing.T) {
			profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
cmd_transform = printf '%s\n' '$(PRIVATE)' | tools/filter > $@
`+target+`: input.txt tools/filter FORCE
	$(call if_changed,transform)
`, map[string]string{"PRIVATE": boundary.value})
			profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.txt", "tools/filter")
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: CompactKbuildInvocationObjectTree,
			}); err != nil {
				t.Fatal(err)
			}
			metadata := &CompactMetadata{
				actionRoles: testConfiguredScopedActionRoles,
				Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
			}
			err := buildCompactKbuildTargetForTest(metadata, compactGenericRecipePlanForTest(), target)
			if err == nil || !strings.Contains(err.Error(), "unlowered private action placeholder") {
				t.Fatalf("compound build error = %v, want private-placeholder rejection before script encoding", err)
			}
		})
	}
}

func TestGenericKbuildCompoundSeparatesLiteralMarkersFromTrustedOutputRoot(t *testing.T) {
	const target = "generated/result.h"
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
cmd_transform = printf '%s\n' '__LINUX_BZL_OBJECT_TREE__' '$${tree:prep}' $(OUTPUT) | tools/filter > $@
`+target+`: input.txt tools/filter FORCE
	$(call if_changed,transform)
`, map[string]string{"OUTPUT": "__LINUX_BZL_OBJECT_TREE__/trusted-output"})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.txt", "tools/filter")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	recipe := plan.Recipes[plan.Nodes[len(plan.Nodes)-1].Recipe]
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	for _, literal := range []string{`'__LINUX_BZL_OBJECT_TREE__'`, `'${tree:prep}'`} {
		if !strings.Contains(script, literal) {
			t.Fatalf("compound script mutated literal marker %q: %q", literal, script)
		}
	}
	if strings.Count(script, "__LINUX_BZL_OBJECT_TREE__") != 1 ||
		!strings.Contains(script, " trusted-output |") {
		t.Fatalf("compound script did not separate literal and trusted output roots: %q", script)
	}
	if !slices.Contains(recipe.Arguments, "-literal_tree_offset") {
		t.Fatalf("mixed marker provenance bindings=%#v", recipe.Arguments)
	}
}

func TestGenericKbuildCompoundProtectsLiteralMarkersFromIncludedMakefile(t *testing.T) {
	const target = "generated/result.h"
	root := t.TempDir()
	mustWriteSource(t, root, "Kbuild", `
include child.mk
`+target+`: input.txt tools/filter FORCE
	$(call if_changed,transform)
`)
	mustWriteSource(t, root, "child.mk", `
cmd_transform = printf '%s\n' '__LINUX_BZL_OBJECT_TREE__' '$${tree:prep}' $(OUTPUT) | tools/filter > $@
`)
	mustWriteSource(t, root, "input.txt", "input\n")
	mustWriteSource(t, root, "tools/filter", "filter\n")
	kb, err := ParseKbuildFileTree(filepath.Join(root, "Kbuild"), KbuildOptions{
		RootDir:                 root,
		SourceRoots:             map[string]string{"__LINUX_BZL_SOURCE_TREE__": root},
		Variables:               map[string]string{"OUTPUT": "__LINUX_BZL_OBJECT_TREE__/trusted-output"},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("build:root", "Kbuild", "", kb)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	recipe := plan.Recipes[plan.Nodes[len(plan.Nodes)-1].Recipe]
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	for _, literal := range []string{`'__LINUX_BZL_OBJECT_TREE__'`, `'${tree:prep}'`} {
		if !strings.Contains(script, literal) {
			t.Fatalf("included command mutated literal marker %q: %q", literal, script)
		}
	}
	if strings.Count(script, "__LINUX_BZL_OBJECT_TREE__") != 1 ||
		!strings.Contains(script, " trusted-output |") ||
		!strings.Contains(script, "> "+target) {
		t.Fatalf("included command did not separate literal and trusted roots: %q", script)
	}
	if !slices.Contains(recipe.Arguments, "-literal_tree_offset") {
		t.Fatalf("included literal marker provenance bindings=%#v", recipe.Arguments)
	}
}

func TestGenericKbuildCompoundPreservesRecipeLineShellBoundaries(t *testing.T) {
	const target = "generated/result.h"
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
cmd_enter = cd nested
cmd_emit = printf '%s' payload | sed 's/payload/result/' > $@
generated/result.h: FORCE
	$(call if_changed,enter)
	$(call if_changed,emit)
`, nil)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("compound node count=%d, want %d: %#v", got, want, plan.Nodes)
	}
	script := compactKbuildRecipeScriptContentForTest(t, plan.Recipes[plan.Nodes[0].Recipe])
	if got, want := strings.Count(script, "(\n"), 2; got != want {
		t.Fatalf("recipe-line subshell count=%d, want %d: %q", got, want, script)
	}
	if !strings.Contains(script, "(\ncd nested\n)\n(\nprintf '%s' payload | sed 's/payload/result/' > "+target+"\n)") {
		t.Fatalf("compound recipe-line boundaries=%q", script)
	}
}

func TestCompactKbuildRecipeLineShellsLowersMakeControlPrefixes(t *testing.T) {
	lines := []string{
		"@set -e; printf linked",
		"+printf recursive",
		"-false",
		"+@-printf ignored",
	}
	want := "(\nset -e; printf linked\n)\n" +
		"(\nprintf recursive\n)\n" +
		"(\n{ false; } || true\n)\n" +
		"(\n{ printf ignored; } || true\n)"
	if got := compactKbuildRecipeLineShells(lines); got != want {
		t.Fatalf("recipe-line control-prefix lowering = %q, want %q", got, want)
	}
}

func TestGenericKbuildCompoundStripsExpandedMakeRecipePrefix(t *testing.T) {
	metadata, target := compactGenericRecipeMetadataForTest(
		t,
		"@set -e; { printf '%s' payload; :; } > $@; printf '%s\\n' savedcmd > .$@.cmd",
		"",
		"generated.stamp",
	)
	plan := compactGenericRecipePlanForTest()
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	recipe := plan.Recipes[plan.Nodes[len(plan.Nodes)-1].Recipe]
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if strings.Contains(script, "@set") || !strings.Contains(script, "set -e;") {
		t.Fatalf("expanded Make recipe prefix was not removed from compound script: %q", script)
	}
}

func TestGenericKbuildModfinalWrapperStripsNestedMakeRecipePrefixes(t *testing.T) {
	const target = "drivers/demo.ko"
	profile := mustCompactKbuildProfileForTest(t, "modfinal", "scripts/Makefile.modfinal", "", `
cmd = @$(if $(cmd_$(1)),set -e; echo '  LD [M]  $@'; $(cmd_$(1)),:)
make-cmd = $(cmd_$(1))
dot-target = $(dir $@).$(notdir $@)
cmd_and_savecmd = $(cmd); printf '%s\n' 'savedcmd_$@ := $(make-cmd)' > $(dot-target).cmd
if-changed-cond = 1
if_changed_except = $(if $(if-changed-cond),$(cmd_and_savecmd),@:)
cmd_ld_ko_o = $(LD) -r -o $@ $<
drivers/demo.ko: drivers/demo.o FORCE
	+$(call if_changed_except,ld_ko_o,vmlinux.o)
`, map[string]string{"LD": KbuildActionRoleToken("target", "ld")})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	inputRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	if _, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "cc", Product: "modules",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/demo.o"}},
	}, inputRecipe); err != nil {
		t.Fatal(err)
	}

	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("target", "modules", "modules")
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing module-link producer %q", producer)
	}
	if node.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("module-link tool = %q, want %q", node.Tool, compactKbuildScriptRunnerRole)
	}
	script := compactKbuildRecipeScriptContentForTest(t, plan.Recipes[node.Recipe])
	if strings.Contains(script, "\n@set") || strings.Contains(script, "\n+set") {
		t.Fatalf("nested Make control prefix reached executable module-link script: %q", script)
	}
	for _, want := range []string{
		"set -e; echo '  LD [M]  " + target + "'",
		"ld -r -o " + target + " drivers/demo.o",
		"savedcmd_" + target + " := ld -r -o " + target + " drivers/demo.o",
		"drivers/.demo.ko.cmd",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("module-link script omits %q: %q", want, script)
		}
	}
}

func compactKbuildRecipeScriptContentForTest(t *testing.T, recipe ActionRecipe) string {
	t.Helper()
	for index := 0; index+1 < len(recipe.Arguments); index++ {
		switch recipe.Arguments[index] {
		case "-script_content":
			return recipe.Arguments[index+1]
		case "-script_content_base64":
			if slices.ContainsFunc(recipe.ArgumentTransforms, func(transform ActionRecipeArgumentTransform) bool {
				return transform.Index == index+1 && transform.Transform == ActionRecipeArgumentTransformContentTemplateBase64
			}) {
				return recipe.Arguments[index+1]
			}
			decoded, err := base64.StdEncoding.DecodeString(recipe.Arguments[index+1])
			if err != nil {
				t.Fatal(err)
			}
			return string(decoded)
		}
	}
	t.Fatalf("recipe has no evaluated script content: %#v", recipe)
	return ""
}

func TestGenericKbuildRecipeSelectsManifestBoundAuxiliaryRole(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
LZ4 = ambient-lz4
cmd_pack = $(LZ4) -c $< > $@
generated/result.lz4: input.txt FORCE
	$(call if_changed,pack)
`, map[string]string{"LZ4": KbuildActionRoleToken("target", "lz4")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.txt")
	metadata := &CompactMetadata{
		actionRoles: testTargetActionRoles("lz4"),
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build("generated/result.lz4")
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok || node.Tool != "lz4" {
		t.Fatalf("manifest-bound auxiliary node=%#v, want lz4", node)
	}
	if recipe := plan.Recipes[node.Recipe]; recipe.Tool != "lz4" || !slices.Equal(recipe.Arguments, []string{"-c", "${source:object:00000000}"}) {
		t.Fatalf("manifest-bound auxiliary recipe=%#v", recipe)
	}
}

func TestGenericKbuildRecipeDefersGeneratedInputShellCalculationToCompoundAction(t *testing.T) {
	const target = "arch/x86/boot/compressed/vmlinux.bin.lz4"
	profile := mustCompactKbuildProfileForTest(t, "build:arch/x86/boot/compressed", "scripts/Makefile.build", "arch/x86/boot/compressed", `
CONFIG_SHELL = sh
LZ4 = ambient-lz4
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
cmd_lz4_with_size = { : $(srctree)/scripts/file-size.sh; cat $(real-prereqs) | $(LZ4) -l -9 - -; $(size_append); } > $@
`+target+`: arch/x86/boot/compressed/vmlinux.bin arch/x86/boot/compressed/vmlinux.relocs FORCE
	$(call if_changed,lz4_with_size)
`, map[string]string{"LZ4": KbuildActionRoleToken("target", "lz4")})
	profile = compactKbuildProfileWithSourcesForTest(
		t, profile,
		"scripts/file-size.sh",
	)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	effects, selected, err := EvaluateCompactKbuildSelectedTargetEffects(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !selected || len(effects.DeferredContentQueries) != 1 {
		t.Fatalf("selected effects = %#v (selected %t), want one deferred query", effects, selected)
	}
	query := effects.DeferredContentQueries[0]
	prerequisites := []string{
		"arch/x86/boot/compressed/vmlinux.bin",
		"arch/x86/boot/compressed/vmlinux.relocs",
	}
	artifacts := make([]CompactKbuildVisibleArtifact, 0, len(prerequisites))
	for _, prerequisite := range prerequisites {
		artifacts = append(artifacts, CompactKbuildVisibleArtifact{
			Path: prerequisite, Profile: profile.Name, Target: prerequisite,
		})
	}
	metadata := &CompactMetadata{
		actionRoles: testTargetActionRoles("lz4"),
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildDeferredContentSelections: []KbuildDeferredContentSelection{{
				Token: query.Token, Profile: query.Origin.Profile, Target: query.Origin.Target,
				Lifecycle: "target", Scope: "target", Stage: "target",
				GeneratedObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts(artifacts),
			}},
		},
	}
	selectionGraph := &compactKbuildSelectionGraph{
		profiles:              map[string]CompactKbuildProfile{profile.Name: profile},
		selectionsByTarget:    map[string][]compactKbuildSelectionKey{},
		forwardingSelections:  map[compactKbuildSelectionKey]bool{},
		materializedProducers: map[compactKbuildSelectionKey]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, metadata: metadata, selectionGraph: selectionGraph,
	}
	prerequisiteIDs := map[string]string{}
	for _, prerequisite := range prerequisites {
		id, err := appendActionPlanNode(plan, ActionPlanNode{
			Stage: "target", Kind: "generate", Tool: "actionfile", Product: "vmlinux",
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: prerequisite}},
		}, ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
			Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
		})
		if err != nil {
			t.Fatal(err)
		}
		prerequisiteIDs[prerequisite] = id
		key := compactKbuildSelectionKey{profile: profile.Name, target: prerequisite, stage: "target"}
		selectionGraph.selectionsByTarget[prerequisite] = []compactKbuildSelectionKey{key}
		selectionGraph.materializedProducers[key] = id
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatalf("build with selected query %q and registry %#v: %v", query.Token, profile.deferredContentQueries, err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing compound producer %q", producer)
	}
	consumerRecipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || consumerRecipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("generated-input shell calculation node=%#v recipe=%#v, want hermetic compound script", node, consumerRecipe)
	}
	var queryNode ActionPlanNode
	for _, candidate := range plan.Nodes {
		if len(candidate.Outputs) == 1 && strings.HasPrefix(candidate.Outputs[0].Path, ".linux-bzl-content/") {
			queryNode = candidate
			break
		}
	}
	if queryNode.ID == "" || queryNode.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("missing hermetic deferred query node: %#v", plan.Nodes)
	}
	if got, want := len(actionPlanNodeInputSetEntriesForTest(t, plan, queryNode)), len(prerequisiteIDs); got != want {
		t.Fatalf("deferred query persistent inputs=%#v, want %d exact generated prerequisites", actionPlanNodeInputSetEntriesForTest(t, plan, queryNode), want)
	}
	for _, prerequisite := range prerequisites {
		got, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, queryNode, prerequisite)
		want := ActionPlanInputSetEntry{
			Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: prerequisite},
			ProducerID: prerequisiteIDs[prerequisite],
		}
		if !found || got != want {
			t.Fatalf("deferred query persistent input for %q = (%#v, %t), want %#v", prerequisite, got, found, want)
		}
	}
	queryRecipe := plan.Recipes[queryNode.Recipe]
	queryScript := compactKbuildRecipeScriptContentForTest(t, queryRecipe)
	for _, marker := range []string{
		"__LINUX_BZL_SOURCE_TREE__",
		"__LINUX_BZL_OBJECT_TREE__",
		compactKbuildActionSourceTreeMarker,
		compactKbuildActionObjectTreeMarker,
	} {
		if strings.Contains(queryScript, marker) {
			t.Fatalf("deferred query script retained raw root marker %q: %q", marker, queryScript)
		}
	}
	for _, want := range []string{
		"dec_size=0;",
		"for F in arch/x86/boot/compressed/vmlinux.bin arch/x86/boot/compressed/vmlinux.relocs",
		"scripts/file-size.sh $F",
		"expr $dec_size + $fsize",
		"printf '%s%03o' '\\\\' $((0x$ch))",
	} {
		if !strings.Contains(queryScript, want) {
			t.Errorf("query script omits %q: %q", want, queryScript)
		}
	}
	consumerScript := compactKbuildRecipeScriptContentForTest(t, consumerRecipe)
	if strings.Contains(consumerScript, kbuildDeferredContentTokenPrefix) || strings.Contains(consumerScript, "$(dec_size=0") || !strings.Contains(consumerScript, "${content:deferred:") {
		t.Fatalf("consumer script did not bind deferred query content: %q", consumerScript)
	}
	if !strings.Contains(consumerScript, "${tree:kernel}/scripts/file-size.sh") {
		t.Fatalf("consumer script omits downstream source-tree placeholder: %q", consumerScript)
	}
	encodedIndex := slices.Index(consumerRecipe.Arguments, "-script_content_base64")
	if encodedIndex < 0 || encodedIndex+1 == len(consumerRecipe.Arguments) || slices.Contains(consumerRecipe.Arguments, "-script_content") {
		t.Fatalf("consumer script arguments=%q, want only base64 content flag", consumerRecipe.Arguments)
	}
	if got, want := consumerRecipe.ArgumentTransforms, []ActionRecipeArgumentTransform{{
		Index: encodedIndex + 1, Transform: ActionRecipeArgumentTransformContentTemplateBase64,
	}}; !slices.Equal(got, want) {
		t.Fatalf("consumer argument transforms=%#v, want %#v", got, want)
	}
	treeArgument := slices.Index(consumerRecipe.Arguments, "-tree")
	if treeArgument < 0 || treeArgument+1 == len(consumerRecipe.Arguments) ||
		consumerRecipe.Arguments[treeArgument+1] != "kernel=${tree:kernel}" ||
		!slices.Contains(consumerRecipe.Trees, "kernel") {
		t.Fatalf("consumer tree arguments=%q trees=%q, want downstream kernel binding", consumerRecipe.Arguments, consumerRecipe.Trees)
	}
	queryEdges := 0
	for _, edge := range node.Inputs {
		if edge.ProducerID == queryNode.ID {
			queryEdges++
		}
	}
	if queryEdges != 1 {
		t.Fatalf("consumer inputs=%#v, want one exact query edge to %s", node.Inputs, queryNode.ID)
	}
	const commandState = "arch/x86/boot/compressed/.vmlinux.bin.lz4.cmd"
	commandStateSlot := -1
	for slot, output := range node.Outputs {
		if output.Path == commandState {
			commandStateSlot = slot
			break
		}
	}
	if commandStateSlot < 0 {
		t.Fatalf("consumer outputs=%#v, want selected cmd_and_savecmd state %q", node.Outputs, commandState)
	}
	if got := consumerRecipe.WorkingOutputs[planOrdinal(commandStateSlot)]; got != commandState {
		t.Fatalf("consumer working outputs=%#v, want %q bound at output slot %d", consumerRecipe.WorkingOutputs, commandState, commandStateSlot)
	}
	if len(consumerRecipe.ContentSubstitutions) != 2 {
		t.Fatalf("consumer content substitutions=%#v, want execution and saved-command placements", consumerRecipe.ContentSubstitutions)
	}
	wantTransforms := map[string]bool{
		ActionRecipeContentTransformMakeShellSingleWord:          false,
		ActionRecipeContentTransformMakeShellSingleQuotedSegment: false,
	}
	for _, substitution := range consumerRecipe.ContentSubstitutions {
		if _, ok := wantTransforms[substitution.Transform]; !ok {
			t.Fatalf("consumer substitution=%#v, want context-typed shell transform", substitution)
		}
		wantTransforms[substitution.Transform] = true
	}
	for transform, found := range wantTransforms {
		if !found {
			t.Errorf("consumer omits deferred-content transform %q: %#v", transform, consumerRecipe.ContentSubstitutions)
		}
	}
	if !slices.Contains(node.AuxiliaryTools, "lz4") || !slices.Contains(consumerRecipe.AuxiliaryTools, "lz4") {
		t.Fatalf("compound script does not bind selected lz4 role: node=%#v recipe=%#v", node, consumerRecipe)
	}
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		t.Fatal(err)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("typed generated-query graph failed plan validation: %v", err)
	}
}

func TestDeferredKbuildShellSingleWordRequiresStaticArgumentWordPlacement(t *testing.T) {
	token := kbuildDeferredContentTokenPrefix + strings.Repeat("a", 64)
	profile := CompactKbuildProfile{deferredContentQueries: map[string]KbuildDeferredContentQuery{
		token: {Token: token, Transform: ActionRecipeContentTransformMakeShellSingleWord},
	}}
	for name, script := range map[string]string{
		"whole word":          "printf " + token,
		"ARM KBSS defsym":     "ld --defsym _kernel_bss_size=" + token + " -T vmlinux.lds -o vmlinux",
		"literal affixes":     "printf prefix-" + token + "-suffix",
		"typed path prefix":   "printf ${tree:kernel}/" + token,
		"single-quoted state": "printf 'savedcmd_vmlinux := ld --defsym _kernel_bss_size=" + token + "'",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateKbuildDeferredShellSingleWordPlacements(profile, script); err != nil {
				t.Fatalf("unquoted argument placement %q: %v", script, err)
			}
		})
	}
	for name, script := range map[string]string{
		"double quoted":       `printf "` + token + `"`,
		"escaped token":       `printf prefix\` + token,
		"dynamic affix":       "printf $prefix" + token,
		"command head":        "prefix" + token + " input",
		"redirect":            "printf data > prefix" + token,
		"placeholder overlap": "printf ${tree:" + token + "}",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateKbuildDeferredShellSingleWordPlacements(profile, script); err == nil {
				t.Fatalf("placement %q succeeded", script)
			}
		})
	}
	placements, err := kbuildDeferredShellSingleWordPlacements(
		"printf 'savedcmd_vmlinux := ld --defsym _kernel_bss_size="+token+"'", token,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(placements) != 1 || placements[0].transform != ActionRecipeContentTransformMakeShellSingleQuotedSegment {
		t.Fatalf("single-quoted deferred placement = %#v", placements)
	}
}

func TestBindDeferredKbuildContentPreservesArmKBSSCompoundWord(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:arch/arm/boot/compressed", "scripts/Makefile.lib", "arch/arm/boot/compressed", "all:\n", nil)
	token := kbuildDeferredContentTokenPrefix + strings.Repeat("b", 64)
	queryProfile := profile
	queryProfile.deferredContentQueries = map[string]KbuildDeferredContentQuery{}
	profile.deferredContentQueries = map[string]KbuildDeferredContentQuery{
		token: {
			Token: token, Command: "printf 4096", Target: "arch/arm/boot/compressed/vmlinux", Profile: queryProfile,
			Transform: ActionRecipeContentTransformMakeShellSingleWord,
		},
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildDeferredContentSelections: []KbuildDeferredContentSelection{{
				Token: token, Profile: profile.Name, Target: "arch/arm/boot/compressed/vmlinux",
				Lifecycle: "target", Scope: "target", Stage: "target",
			}},
		},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, metadata: metadata,
	}
	const prefix = "ld --defsym _kernel_bss_size="
	const suffix = " -T vmlinux.lds -o arch/arm/boot/compressed/vmlinux"
	consumerID, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "actionfile", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "arch/arm/boot/compressed/vmlinux"}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", "-line", prefix + token + suffix},
		Outputs:   []string{"00000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, ok := compactKbuildPlanNode(plan, consumerID)
	if !ok {
		t.Fatalf("missing ARM vmlinux consumer %s", consumerID)
	}
	bound := plan.Recipes[consumer.Recipe]
	const placeholder = "${content:deferred:00000000}"
	if got, want := bound.Arguments[3], prefix+placeholder+suffix; got != want {
		t.Fatalf("bound ARM KBSS linker word = %q, want %q", got, want)
	}
	if got, want := bound.ContentSubstitutions, map[string]ActionRecipeContentSubstitution{
		"deferred:00000000": {
			Input: "input:content:00000000", Transform: ActionRecipeContentTransformMakeShellSingleWord,
		},
	}; !maps.Equal(got, want) {
		t.Fatalf("ARM KBSS content substitutions = %#v, want %#v", got, want)
	}
	quoted, err := QuoteActionRecipeMakeShellSingleWord("4096\n")
	if err != nil {
		t.Fatal(err)
	}
	materialized := strings.ReplaceAll(bound.Arguments[3], placeholder, quoted)
	tokens, err := lexCompactKbuildRecipe(materialized)
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, len(tokens))
	for _, token := range tokens {
		values = append(values, token.value)
	}
	if got, want := values, []string{
		"ld", "--defsym", "_kernel_bss_size=4096", "-T", "vmlinux.lds", "-o", "arch/arm/boot/compressed/vmlinux",
	}; !slices.Equal(got, want) {
		t.Fatalf("materialized ARM KBSS argv = %#v, want %#v (script %q)", got, want, materialized)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("ARM KBSS deferred-content plan validation: %v", err)
	}
}

func TestDeferredKbuildSelectionDigestIncludesExactProducerInstance(t *testing.T) {
	token := kbuildDeferredContentTokenPrefix + strings.Repeat("b", 64)
	query := KbuildDeferredContentQuery{Token: token}
	first := KbuildDeferredContentSelection{
		Token: token, Lifecycle: "prep", Scope: "target", Stage: "bootstrap",
		GeneratedObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{{
			Path: "generated/input", Profile: "build:root", Target: "first",
		}}),
	}
	second := first
	second.GeneratedObjectTreeArtifacts = EncodeCompactKbuildInitialObjectTreeArtifacts([]CompactKbuildVisibleArtifact{{
		Path: "generated/input", Profile: "build:root", Target: "versioned",
	}})
	if firstDigest, secondDigest := kbuildDeferredContentSelectionDigest(query, first), kbuildDeferredContentSelectionDigest(query, second); firstDigest == secondDigest {
		t.Fatalf("different exact producer selections share digest %q", firstDigest)
	}
}

func TestBindDeferredKbuildContentSupportsEveryPhysicalStage(t *testing.T) {
	tests := []struct {
		stage, scope, consumerTree, scratchTree, product string
	}{
		{stage: "prehost", scope: "host", consumerTree: "prehost", scratchTree: "prehost", product: "sdk"},
		{stage: "bootstrap", scope: "target", consumerTree: "bootstrap", scratchTree: "bootstrap", product: "sdk"},
		{stage: "host", scope: "host", consumerTree: "host", scratchTree: "host", product: "sdk"},
		{stage: "prep", scope: "target", consumerTree: "prep", scratchTree: "prep", product: "sdk"},
		{stage: "target", scope: "target", consumerTree: "objects", scratchTree: "metadata", product: "vmlinux"},
	}
	covered := map[string]bool{}
	for _, test := range tests {
		covered[test.stage] = true
		t.Run(test.stage, func(t *testing.T) {
			profile := mustCompactKbuildProfileForTest(t, "deferred-stage", "Makefile", "", "all:\n", nil)
			token := kbuildDeferredContentTokenPrefix + strings.Repeat("c", 64)
			queryProfile := profile
			queryProfile.deferredContentQueries = map[string]KbuildDeferredContentQuery{}
			profile.deferredContentQueries = map[string]KbuildDeferredContentQuery{
				token: {
					Token: token, Command: "printf value", Target: "all", Profile: queryProfile,
					Transform: ActionRecipeContentTransformMakeShellWord,
				},
			}
			metadata := &CompactMetadata{
				actionRoles: testConfiguredScopedActionRoles,
				Config: CompactConfig{
					KbuildProfiles: []CompactKbuildProfile{profile},
					KbuildDeferredContentSelections: []KbuildDeferredContentSelection{{
						Token: token, Profile: profile.Name, Target: "all",
						Lifecycle: "target", Scope: test.scope, Stage: test.stage,
					}},
				},
			}
			plan := &ActionPlan{
				Toolsets: map[string]string{
					"target": actionPlanTestProbeIdentity,
					"host":   actionPlanTestProbeIdentity,
				},
				Recipes: map[string]ActionRecipe{}, metadata: metadata,
			}
			consumerID, err := appendActionPlanNode(plan, ActionPlanNode{
				Stage: test.stage, Kind: "generate", Tool: "actionfile", Product: test.product,
				Outputs: []ActionPlanOutput{{Tree: test.consumerTree, Path: "consumer-" + test.stage}},
			}, ActionRecipe{
				Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
				Arguments: []string{
					"-out", "${output:00000000}",
					"-line", "value=" + token,
				},
				Outputs: []string{"00000000"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Nodes) != 2 {
				t.Fatalf("nodes = %#v, want deferred query and consumer", plan.Nodes)
			}
			consumer, ok := compactKbuildPlanNode(plan, consumerID)
			if !ok {
				t.Fatalf("missing consumer %s", consumerID)
			}
			var query ActionPlanNode
			for _, node := range plan.Nodes {
				if node.ID != consumerID {
					query = node
				}
			}
			if query.Stage != test.stage || query.Product != "sdk" || len(query.Outputs) != 1 ||
				query.Outputs[0].Tree != test.scratchTree || !strings.HasPrefix(query.Outputs[0].Path, ".linux-bzl-content/") {
				t.Fatalf("deferred %s query node = %#v, want %s scratch tree", test.stage, query, test.scratchTree)
			}
			if got, want := consumer.Inputs, []ActionPlanNodeEdge{{Role: "content", ProducerID: query.ID, Slot: 0}}; !slices.Equal(got, want) {
				t.Fatalf("consumer inputs = %#v, want %#v", got, want)
			}
			bound := plan.Recipes[consumer.Recipe]
			if strings.Contains(strings.Join(bound.Arguments, " "), token) ||
				!slices.Contains(bound.Arguments, "value=${content:deferred:00000000}") {
				t.Fatalf("bound consumer arguments = %q", bound.Arguments)
			}
			if got, want := bound.Inputs, []string{"content:00000000"}; !slices.Equal(got, want) {
				t.Fatalf("bound consumer inputs = %q, want %q", got, want)
			}
			if got, want := bound.ContentSubstitutions, map[string]ActionRecipeContentSubstitution{
				"deferred:00000000": {
					Input: "input:content:00000000", Transform: ActionRecipeContentTransformMakeShellWord,
				},
			}; !maps.Equal(got, want) {
				t.Fatalf("content substitutions = %#v, want %#v", got, want)
			}
			if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
				t.Fatalf("%s deferred-content plan validation: %v", test.stage, err)
			}
		})
	}
	for stage := range LinuxKernelPlanStages {
		if !covered[stage] {
			t.Errorf("physical stage %q has no deferred-content binding case", stage)
		}
	}
	if tree, ok := actionPlanStageScratchTree("unknown"); ok || tree != "" {
		t.Fatalf("unknown stage scratch tree = (%q, %t), want unsupported", tree, ok)
	}
}

func TestDeferredKbuildContentQueryIsSharedAcrossConsumerStagesAndProducts(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "deferred-cross-stage", "Makefile", "", "all:\n", nil)
	token := kbuildDeferredContentTokenPrefix + strings.Repeat("d", 64)
	queryProfile := profile
	queryProfile.deferredContentQueries = map[string]KbuildDeferredContentQuery{}
	profile.deferredContentQueries = map[string]KbuildDeferredContentQuery{
		token: {
			Token: token, Command: "printf value", Target: "all", Profile: queryProfile,
			Transform: ActionRecipeContentTransformMakeShellWord,
		},
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config: CompactConfig{
			KbuildProfiles: []CompactKbuildProfile{profile},
			KbuildDeferredContentSelections: []KbuildDeferredContentSelection{{
				Token: token, Profile: profile.Name, Target: "all",
				Lifecycle: "prep", Scope: "target", Stage: "bootstrap",
			}},
		},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"target": actionPlanTestProbeIdentity,
			"host":   actionPlanTestProbeIdentity,
		},
		Recipes: map[string]ActionRecipe{}, metadata: metadata,
	}
	consumers := []struct {
		stage, tree, product string
	}{
		{stage: "bootstrap", tree: "bootstrap", product: "sdk"},
		{stage: "host", tree: "host", product: "sdk"},
		{stage: "prep", tree: "prep", product: "sdk"},
		{stage: "target", tree: "objects", product: "vmlinux"},
	}
	queryIDs := map[string]bool{}
	for _, consumer := range consumers {
		consumerID, err := appendActionPlanNode(plan, ActionPlanNode{
			Stage: consumer.stage, Kind: "generate", Tool: "actionfile", Product: consumer.product,
			Outputs: []ActionPlanOutput{{Tree: consumer.tree, Path: "consumer-" + consumer.stage}},
		}, ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
			Arguments: []string{
				"-out", "${output:00000000}",
				"-line", "value=" + token,
			},
			Outputs: []string{"00000000"},
		})
		if err != nil {
			t.Fatal(err)
		}
		node, ok := compactKbuildPlanNode(plan, consumerID)
		if !ok || len(node.Inputs) != 1 || node.Inputs[0].Role != "content" {
			t.Fatalf("%s consumer node = %#v, want one content edge", consumer.stage, node)
		}
		queryIDs[node.Inputs[0].ProducerID] = true
	}
	if len(queryIDs) != 1 {
		t.Fatalf("consumer stages/products bound different query nodes: %#v", queryIDs)
	}
	for queryID := range queryIDs {
		query, ok := compactKbuildPlanNode(plan, queryID)
		if !ok || query.Stage != "bootstrap" || query.Product != "sdk" || len(query.Outputs) != 1 ||
			query.Outputs[0].Tree != "bootstrap" || !strings.HasPrefix(query.Outputs[0].Path, ".linux-bzl-content/bootstrap/") {
			t.Fatalf("shared deferred-content query = %#v", query)
		}
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("cross-stage deferred-content plan validation: %v", err)
	}
}

func TestGenericKbuildRecipeResolvesScopeNeutralUtilityFromActionStage(t *testing.T) {
	const target = "generated/table.c"
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
cmd_generate = $(AWK) -f generate.awk input.txt > $@
generated/table.c: generate.awk input.txt FORCE
	$(call if_changed,generate)
`, map[string]string{"AWK": KbuildActionRoleToken(KbuildActionRoleAutoScope, "awk")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "generate.awk", "input.txt")
	metadata := &CompactMetadata{
		actionRoles: testScopedActionRoles("awk"),
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	for _, test := range []struct {
		stage, tree, product string
	}{
		{stage: "target", tree: "objects", product: "vmlinux"},
		{stage: "host", tree: "host", product: "sdk"},
	} {
		t.Run(test.stage, func(t *testing.T) {
			plan := &ActionPlan{
				Toolsets: map[string]string{
					"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity,
				},
				Recipes: map[string]ActionRecipe{},
			}
			builder := newCompactKbuildRulePlanBuilder(metadata, plan).
				forProfile(profile).
				forOutput(test.stage, test.tree, test.product)
			producer, err := builder.build(target)
			if err != nil {
				t.Fatal(err)
			}
			node, ok := compactKbuildPlanNode(plan, producer)
			if !ok {
				t.Fatalf("%s producer %q not found", test.stage, producer)
			}
			recipe := plan.Recipes[node.Recipe]
			if node.Stage != test.stage || node.Tool != "awk" || node.Outputs[0] != (ActionPlanOutput{Tree: test.tree, Path: target}) {
				t.Fatalf("%s scope-neutral utility node = %#v", test.stage, node)
			}
			if recipe.Tool != "awk" || len(recipe.ExecutableInputs) != 0 || strings.Contains(strings.Join(recipe.Arguments, " "), kbuildActionRoleTokenPrefix) {
				t.Fatalf("%s scope-neutral utility recipe = %#v", test.stage, recipe)
			}
		})
	}
}

func TestGenericKbuildRecipeRejectsBareConfiguredAuxiliaryRole(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
cmd_pack = lz4 -c $< > $@
generated/result.lz4: input.txt FORCE
	$(call if_changed,pack)
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.txt")
	metadata := &CompactMetadata{
		actionRoles: testTargetActionRoles("lz4"),
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, &ActionPlan{Recipes: map[string]ActionRecipe{}}).forProfile(profile)
	_, err := builder.build("generated/result.lz4")
	if err == nil || !strings.Contains(err.Error(), "without scoped source provenance") {
		t.Fatalf("bare configured auxiliary error=%v", err)
	}
}

func TestGenericKbuildRecipeProxiesConfiguredToolsInArgumentsAndEnvironment(t *testing.T) {
	const (
		target = "generated/result.o"
		source = "input.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
cmd_transform = DRIVER=$(CC) COPIER=$(OBJCOPY) $(CC) --driver=$(CC) --plugin=$(OBJCOPY) -c -o $@ $<
generated/result.o: input.c FORCE
	$(call if_changed,transform)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc"), "OBJCOPY": KbuildActionRoleToken("target", "objcopy")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("node count=%d, want %d: %#v", got, want, plan.Nodes)
	}
	node := plan.Nodes[0]
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != "cc" || recipe.Tool != "cc" {
		t.Fatalf("configured primary tool node=%#v recipe=%#v", node, recipe)
	}
	if got, want := recipe.Environment["DRIVER"], "${tool:cc}"; got != want {
		t.Fatalf("primary tool environment=%q, want %q", got, want)
	}
	if got, want := recipe.Environment["COPIER"], "${tool:objcopy}"; got != want {
		t.Fatalf("auxiliary tool environment=%q, want %q", got, want)
	}
	if !slices.Contains(recipe.Arguments, "--driver=${tool:cc}") || !slices.Contains(recipe.Arguments, "--plugin=${tool:objcopy}") {
		t.Fatalf("configured tool argument missing from %q", recipe.Arguments)
	}
	if !slices.Equal(node.AuxiliaryTools, []string{"objcopy"}) || !slices.Equal(recipe.AuxiliaryTools, []string{"objcopy"}) {
		t.Fatalf("auxiliary tools node=%q recipe=%q, want objcopy only", node.AuxiliaryTools, recipe.AuxiliaryTools)
	}
}

func TestGenericKbuildRecipePublishesTargetAfterTrailingValidation(t *testing.T) {
	const (
		target = "generated/result.so"
		source = "input.o"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
cmd_link_and_check = $(LD) -o $@ $<; $(OBJCOPY) --verify $@
generated/result.so: input.o FORCE
	$(call if_changed,link_and_check)
`, map[string]string{"LD": KbuildActionRoleToken("target", "ld"), "OBJCOPY": KbuildActionRoleToken("target", "objcopy")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 2; got != want {
		t.Fatalf("node count=%d, want %d: %#v", got, want, plan.Nodes)
	}
	link, validate := plan.Nodes[0], plan.Nodes[1]
	if link.Tool != "ld" || link.Outputs[0].Path != target ||
		!strings.HasPrefix(link.Outputs[0].ArtifactPath, ".linux-bzl-intermediate/") ||
		actionPlanOutputIsCanonical(link.Outputs[0]) {
		t.Fatalf("private link output=%#v", link)
	}
	validationRecipe := plan.Recipes[validate.Recipe]
	if validate.Tool != "objcopy" || validate.Outputs[0].Path != target {
		t.Fatalf("validation node=%#v", validate)
	}
	if got, want := len(validate.Outputs), 1; got != want {
		t.Fatalf("validation outputs=%#v, want only the carried target", validate.Outputs)
	}
	if validationRecipe.Stdout != "" || validationRecipe.WorkingOutputs["00000000"] != target {
		t.Fatalf("validation recipe=%#v", validationRecipe)
	}
	if !slices.Contains(sortedStringMapValues(validationRecipe.WorkingInputs), target) ||
		!slices.ContainsFunc(validate.Inputs, func(input ActionPlanNodeEdge) bool { return input.ProducerID == link.ID }) {
		t.Fatalf("validation does not carry private link output: node=%#v recipe=%#v", validate, validationRecipe)
	}

	observedPlan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	observedBuilder := newCompactKbuildRulePlanBuilder(metadata, observedPlan).forProfile(profile)
	observedBuilder, err := observedBuilder.forObservedOutputs(target, []compactKbuildObservedOutput{{
		output: ActionPlanOutput{Tree: "metadata", Path: ".captures/trailing-validation"},
		path:   "generated/opaque.validation",
	}})
	if err != nil {
		t.Fatal(err)
	}
	observedProducer, err := observedBuilder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(observedPlan.Nodes), 2; got != want {
		t.Fatalf("observed validation node count = %d, want split link and validation: %#v", got, observedPlan.Nodes)
	}
	observedLink, observedValidation := observedPlan.Nodes[0], observedPlan.Nodes[1]
	if observedValidation.ID != observedProducer {
		t.Fatalf("observed producer = %q, want final validation %q", observedProducer, observedValidation.ID)
	}
	stateSlot := func(node ActionPlanNode) int {
		for slot, output := range node.Outputs {
			if output.ObservedPath == "generated/opaque.validation" {
				return slot
			}
		}
		return -1
	}
	linkStateSlot, validationStateSlot := stateSlot(observedLink), stateSlot(observedValidation)
	if linkStateSlot < 0 || validationStateSlot < 0 ||
		!strings.HasPrefix(observedLink.Outputs[linkStateSlot].Path, compactKbuildSideOutputStateDirectory+"/commands/") ||
		observedValidation.Outputs[validationStateSlot].Path != ".captures/trailing-validation" {
		t.Fatalf("observed validation states: link=%#v validation=%#v", observedLink.Outputs, observedValidation.Outputs)
	}
	observedValidationRecipe := observedPlan.Recipes[observedValidation.Recipe]
	baseBindings := observedValidationRecipe.ObservedOutputBases[planOrdinal(validationStateSlot)]
	if len(baseBindings) != 1 {
		t.Fatalf("observed validation bases = %q, want link state", baseBindings)
	}
	baseIndex := slices.Index(observedValidationRecipe.Inputs, baseBindings[0])
	if baseIndex < 0 || baseIndex >= len(observedValidation.Inputs) {
		t.Fatalf("observed validation base %q is not an input: node=%#v recipe=%#v", baseBindings[0], observedValidation, observedValidationRecipe)
	}
	base := observedValidation.Inputs[baseIndex]
	if base.Role != "observed-state" || base.ProducerID != observedLink.ID || base.Slot != linkStateSlot {
		t.Fatalf("observed validation base = %#v, want link %s slot %d", base, observedLink.ID, linkStateSlot)
	}
	if _, err := observedPlan.entries(); err != nil {
		t.Fatalf("observed validation plan is invalid: %v", err)
	}
}

func TestCompactKbuildRecipeIntermediateArtifactPathIsStableAndContextUnique(t *testing.T) {
	const (
		target = "rust/LINUX_BZL_PROBE_deadbeef"
		output = "rust/pin_init.o"
	)
	profile := CompactKbuildProfile{Name: "build:rust-host", Path: "scripts/Makefile.build"}
	key := compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "host"}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, &ActionPlan{}).
		forOutput("host", "host", "sdk").
		forSelection(key, profile)
	first := builder.compactKbuildRecipeIntermediateArtifactPath(profile, target, 2, output)
	second := builder.compactKbuildRecipeIntermediateArtifactPath(profile, target, 2, output)
	if first != second || !strings.HasPrefix(first, ".linux-bzl-intermediate/") || !strings.HasSuffix(first, ".o") {
		t.Fatalf("stable intermediate artifact paths = %q and %q", first, second)
	}

	targetProfile := profile
	targetProfile.Name = "build:rust-target"
	targetKey := compactKbuildSelectionKey{profile: targetProfile.Name, target: target, stage: "target"}
	targetBuilder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, &ActionPlan{}).
		forOutput("target", "objects", "sdk").
		forSelection(targetKey, targetProfile)
	targetArtifact := targetBuilder.compactKbuildRecipeIntermediateArtifactPath(targetProfile, target, 2, output)
	if targetArtifact == first {
		t.Fatalf("host and target contexts share intermediate artifact %q", first)
	}

	otherProfile := profile
	otherProfile.Name = "build:rust-other"
	otherKey := compactKbuildSelectionKey{profile: otherProfile.Name, target: target, stage: "host"}
	otherBuilder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, &ActionPlan{}).
		forOutput("host", "host", "sdk").
		forSelection(otherKey, otherProfile)
	otherArtifact := otherBuilder.compactKbuildRecipeIntermediateArtifactPath(otherProfile, target, 2, output)
	if otherArtifact == first || otherArtifact == targetArtifact {
		t.Fatalf("distinct profile reused intermediate artifact %q", otherArtifact)
	}
}

func TestGenericKbuildRustSnapshotsKeepLogicalOutputAndDistinctPhysicalArtifacts(t *testing.T) {
	target := "rust/" + linuxProbeSymbolPrefix + strings.Repeat("1", 64)
	const source = "rust/pin_init.rs"
	profileForScope := func(scope string) CompactKbuildProfile {
		t.Helper()
		profile := mustCompactKbuildProfileForTest(t, "build:rust-"+scope, "scripts/Makefile.build", "rust", `
cmd_rustc = $(RUSTC) --emit=obj=$@ $<
cmd_check = $(OBJCOPY) --verify $@
$(obj)/LINUX_BZL_PROBE_%: $(src)/pin_init.rs FORCE
	$(call if_changed,rustc)
	$(call cmd,check)
`, map[string]string{
			"RUSTC":   KbuildActionRoleToken(scope, "rustc"),
			"OBJCOPY": KbuildActionRoleToken(scope, "objcopy"),
			"obj":     "rust",
			"src":     "rust",
		})
		return compactKbuildProfileWithSourcesForTest(t, profile, source)
	}
	hostProfile := profileForScope("host")
	targetProfile := profileForScope("target")
	hostSelection := CompactKbuildSelection{
		Profile: hostProfile.Name, Target: target, MakeTarget: target, Scope: "host", Lifecycle: "target", Stage: "host",
	}
	targetSelection := CompactKbuildSelection{
		Profile: targetProfile.Name, Target: target, MakeTarget: target, Scope: "target", Lifecycle: "target", Stage: "target",
	}
	metadata := &CompactMetadata{
		actionRoles: append(testConfiguredScopedActionRoles, testScopedActionRoles("rustc")...),
		Config: CompactConfig{
			KbuildProfiles:   []CompactKbuildProfile{hostProfile, targetProfile},
			KbuildSelections: []CompactKbuildSelection{hostSelection, targetSelection},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(metadata.Config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity,
		},
		Recipes:        map[string]ActionRecipe{},
		selectionGraph: graph,
	}
	for _, snapshot := range []struct {
		profile CompactKbuildProfile
		stage   string
		tree    string
	}{
		{profile: hostProfile, stage: "host", tree: "host"},
		{profile: targetProfile, stage: "target", tree: "objects"},
	} {
		key := compactKbuildSelectionKey{
			profile: snapshot.profile.Name, target: target, stage: snapshot.stage,
		}
		builder := newCompactKbuildRulePlanBuilder(metadata, plan).
			forOutput(snapshot.stage, snapshot.tree, "sdk").
			forSelection(key, snapshot.profile)
		if _, err := builder.build(target); err != nil {
			t.Fatalf("build %s Rust snapshot: %v", snapshot.stage, err)
		}
	}

	physical := map[string]string{}
	for _, node := range plan.Nodes {
		if node.Tool != "rustc" {
			continue
		}
		if len(node.Outputs) != 1 || node.Outputs[0].Path != target ||
			!strings.HasPrefix(node.Outputs[0].ArtifactPath, ".linux-bzl-intermediate/") ||
			actionPlanOutputIsCanonical(node.Outputs[0]) {
			t.Fatalf("%s Rust compiler output = %#v, want logical target plus private artifact", node.Stage, node.Outputs)
		}
		artifact := actionPlanOutputArtifactPath(node.Outputs[0])
		if previous := physical[artifact]; previous != "" {
			t.Fatalf("Rust snapshots %s and %s share physical output %q", previous, node.Stage, artifact)
		}
		physical[artifact] = node.Stage
	}
	if len(physical) != 2 {
		t.Fatalf("Rust compiler physical outputs = %#v, want host and target snapshots", physical)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("Rust snapshot plan: %v", err)
	}
}

func TestSpecialCommandNamesUseSourceDrivenGenericLowering(t *testing.T) {
	for _, command := range []string{
		"ar_builtin", "ar_vmlinux.a", "gzip", "image", "zoffset", "voffset", "relocs", "vdso_and_check",
	} {
		t.Run(command, func(t *testing.T) {
			const (
				target = "generated/result.o"
				source = "input.c"
			)
			profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", fmt.Sprintf(`
cmd_%[1]s = $(CC) -DSOURCE_DRIVEN -c -o $@ $<
cmd_vdso_check = deliberately-present
generated/result.o: input.c FORCE
	$(call if_changed,%[1]s)
`, command), map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
			profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
			metadata := &CompactMetadata{
				actionRoles: testConfiguredScopedActionRoles,
				Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
			}
			plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
			if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
				t.Fatal(err)
			}
			if got, want := len(plan.Nodes), 1; got != want {
				t.Fatalf("node count=%d, want %d: %#v", got, want, plan.Nodes)
			}
			node := plan.Nodes[0]
			recipe := plan.Recipes[node.Recipe]
			if node.Tool != "cc" || node.Kind != "compile" || !slices.Contains(recipe.Arguments, "-DSOURCE_DRIVEN") {
				t.Fatalf("command %q used non-generic lowering: node=%#v recipe=%#v", command, node, recipe)
			}
		})
	}
}

func TestImplicitRuleSearchStopsAtExplicitOughtToExistTarget(t *testing.T) {
	const directory = "arch/x86/entry/vdso"
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
cmd_ar_builtin = $(AR) rc $@ $<
cmd_cc_o_c = $(CC) -c -o $@ $<
cmd_vdso_source = $(CC) -E -o $@ $<
cmd_vdso = $(LD) -o $@ $^
$(obj)/built-in.a: $(obj)/vdso-image-64.o FORCE
	$(call if_changed,ar_builtin)
$(obj)/%.o: $(obj)/%.c FORCE
	$(call if_changed,cc_o_c)
$(obj)/vdso-image-%.c: $(obj)/vdso%.so.dbg FORCE
	$(call if_changed,vdso_source)
$(obj)/vdso64.so.dbg: $(obj)/vclock_gettime.o FORCE
	$(call if_changed,vdso)
`, map[string]string{
		"AR": KbuildActionRoleToken("target", "ar"), "CC": KbuildActionRoleToken("target", "cc"), "LD": KbuildActionRoleToken("target", "ld"), "obj": directory,
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, directory+"/vclock_gettime.c")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, directory+"/built-in.a"); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{directory + "/vdso-image-64.o", directory + "/vclock_gettime.o"} {
		if _, _, ok := planProducerByOutput(plan, "objects", output); !ok {
			t.Fatalf("plan omits %q after explicit implicit-search boundary: %#v", output, plan.Nodes)
		}
	}
}

func TestGenericKbuildPatternRulePreservesParentTraversalTargetSpelling(t *testing.T) {
	const (
		directory = "arch/x86/kvm"
		target    = directory + "/built-in.a"
		object    = "virt/kvm/kvm_main.o"
		source    = "virt/kvm/kvm_main.c"
		header    = "virt/kvm/kvm_main.h"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
target-stem = $(basename $(patsubst $(obj)/%,%,$@))
stem_cflags = $(CFLAGS_$(target-stem).o)
CFLAGS_../../../virt/kvm/kvm_main.o = -DLEXICAL_TARGET_STEM
CFLAGS_virt/kvm/kvm_main.o = -DCANONICAL_TARGET_STEM_MUST_NOT_LEAK
cmd_ar_builtin = $(AR) rc $@ $^
cmd_cc_o_c = $(CC) $(object_cflags) $(stem_cflags) -DRELATIVE_OBJECT=$(patsubst $(obj)/%,%,$@) -c -o $@ $<
$(obj)/built-in.a: $(obj)/../../../virt/kvm/kvm_main.o FORCE
	$(call if_changed,ar_builtin)
$(obj)/%.o: private object_cflags = -DLEXICAL_KVM_TARGET
virt/kvm/kvm_main.o: private object_cflags += -DCANONICAL_ALIAS_MUST_NOT_LEAK
$(obj)/../../../virt/kvm/kvm_main.o: $(obj)/../../../virt/kvm/kvm_main.h
$(obj)/%.o: $(obj)/%.c FORCE
	$(call if_changed_dep,cc_o_c)
`, map[string]string{
		"AR":  KbuildActionRoleToken("target", "ar"),
		"CC":  KbuildActionRoleToken("target", "cc"),
		"obj": directory,
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source, header)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	objectProducer, _, ok := planProducerByOutput(plan, "objects", object)
	if !ok {
		t.Fatalf("plan omits canonical cross-directory object %q: %#v", object, plan.Nodes)
	}
	objectNode, ok := compactKbuildPlanNode(plan, objectProducer)
	if !ok {
		t.Fatalf("plan omits canonical cross-directory producer %q", objectProducer)
	}
	objectRecipe := plan.Recipes[objectNode.Recipe]
	if !slices.Contains(objectRecipe.Arguments, "-DLEXICAL_KVM_TARGET") {
		t.Fatalf("cross-directory compile arguments omit lexical target-specific flag: %q", objectRecipe.Arguments)
	}
	if slices.Contains(objectRecipe.Arguments, "-DCANONICAL_ALIAS_MUST_NOT_LEAK") {
		t.Fatalf("cross-directory compile inherited a distinct canonical target-specific assignment: %q", objectRecipe.Arguments)
	}
	if !slices.Contains(objectRecipe.Arguments, "-DLEXICAL_TARGET_STEM") {
		t.Fatalf("cross-directory compile arguments omit source-derived lexical target-stem flag: %q", objectRecipe.Arguments)
	}
	if slices.Contains(objectRecipe.Arguments, "-DCANONICAL_TARGET_STEM_MUST_NOT_LEAK") {
		t.Fatalf("cross-directory compile derived target-stem from canonical alias: %q", objectRecipe.Arguments)
	}
	if !slices.Contains(objectRecipe.Arguments, "-DRELATIVE_OBJECT=../../../virt/kvm/kvm_main.o") {
		t.Fatalf("cross-directory compile arguments lost lexical automatic target: %q", objectRecipe.Arguments)
	}
	if len(objectNode.Sources) != 2 {
		t.Fatalf("cross-directory compile sources = %#v, want source plus exact prerequisite-only header", objectNode.Sources)
	}
	if _, _, ok := planProducerByOutput(plan, "objects", target); !ok {
		t.Fatalf("plan omits archive %q: %#v", target, plan.Nodes)
	}
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			if strings.Contains(output.Path, "..") || strings.Contains(output.ArtifactPath, "..") {
				t.Fatalf("cross-directory plan leaks lexical traversal into output %#v", output)
			}
		}
	}
}

func TestGenericKbuildSelectedTargetMaterializesParentTraversalMakeTarget(t *testing.T) {
	const (
		directory  = "arch/x86/kvm"
		target     = "virt/kvm/kvm_main.o"
		makeTarget = directory + "/../../../virt/kvm/kvm_main.o"
		source     = "virt/kvm/kvm_main.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
target-stem = $(basename $(patsubst $(obj)/%,%,$@))
stem_cflags = $(CFLAGS_$(target-stem).o)
CFLAGS_../../../virt/kvm/kvm_main.o = -DLEXICAL_TARGET_STEM
CFLAGS_virt/kvm/kvm_main.o = -DCANONICAL_TARGET_STEM_MUST_NOT_LEAK
cmd_cc_o_c = $(CC) $(object_cflags) $(stem_cflags) -DRELATIVE_OBJECT=$(patsubst $(obj)/%,%,$@) -c -o $@ $<
$(obj)/%.o: private object_cflags = -DLEXICAL_KVM_TARGET
virt/kvm/kvm_main.o: private object_cflags += -DCANONICAL_ALIAS_MUST_NOT_LEAK
$(obj)/%.o: $(obj)/%.c FORCE
	$(call if_changed_dep,cc_o_c)
`, map[string]string{
		"CC":  KbuildActionRoleToken("target", "cc"),
		"obj": directory,
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	if _, matched, err := metadata.compactKbuildRuleForProfile(profile, target); err != nil {
		t.Fatal(err)
	} else if matched {
		t.Fatalf("canonical target %q unexpectedly matched the lexical $(obj)/%%.o rule", target)
	}

	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildSelectedTarget(target, makeTarget)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("selected target producer %q is absent", producer)
	}
	if got, want := node.Outputs, []ActionPlanOutput{{Tree: "objects", Path: target}}; !slices.Equal(got, want) {
		t.Fatalf("selected target outputs = %#v, want canonical %#v", got, want)
	}
	recipe := plan.Recipes[node.Recipe]
	for _, argument := range []string{
		"-DLEXICAL_KVM_TARGET",
		"-DLEXICAL_TARGET_STEM",
		"-DRELATIVE_OBJECT=../../../virt/kvm/kvm_main.o",
	} {
		if !slices.Contains(recipe.Arguments, argument) {
			t.Fatalf("selected target arguments omit %q: %q", argument, recipe.Arguments)
		}
	}
	for _, argument := range []string{
		"-DCANONICAL_ALIAS_MUST_NOT_LEAK",
		"-DCANONICAL_TARGET_STEM_MUST_NOT_LEAK",
	} {
		if slices.Contains(recipe.Arguments, argument) {
			t.Fatalf("selected target arguments leaked canonical alias flag %q: %q", argument, recipe.Arguments)
		}
	}
}

func TestGenericKbuildSelectedSourceScriptMaterializesParentTraversalMakeTarget(t *testing.T) {
	const (
		directory  = "arch/x86/boot/compressed"
		target     = "arch/x86/boot/voffset.h"
		makeTarget = directory + "/../voffset.h"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "arch/x86/boot/compressed/Makefile", directory, `
obj := arch/x86/boot/compressed
$(obj)/../voffset.h: FORCE
	printf '%s\n' '#define VO_TEST' | sed -n 'p' > $@
`, nil)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildSelectedTarget(target, makeTarget)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("selected source-script producer %q is absent", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("selected source-script node=%#v recipe=%#v", node, recipe)
	}
	if got, want := node.Outputs, []ActionPlanOutput{{Tree: "objects", Path: target}}; !slices.Equal(got, want) {
		t.Fatalf("selected source-script outputs = %#v, want canonical %#v", got, want)
	}
	if got := recipe.WorkingOutputs["00000000"]; got != target {
		t.Fatalf("selected source-script working output = %q, want canonical %q", got, target)
	}
	if got, want := recipe.WorkingDirectories, []string{directory}; !slices.Equal(got, want) {
		t.Fatalf("selected source-script working directories = %q, want lexical anchor %q", got, want)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if !strings.Contains(script, "> "+makeTarget) {
		t.Fatalf("selected source script = %q, want lexical automatic target %q", script, makeTarget)
	}
	for _, output := range node.Outputs {
		if strings.Contains(output.Path, "..") || strings.Contains(output.ArtifactPath, "..") {
			t.Fatalf("selected source-script output leaks lexical traversal: %#v", output)
		}
	}
}

func TestCompactKbuildLexicalTraversalWorkingDirectories(t *testing.T) {
	for _, test := range []struct {
		name      string
		lexical   string
		graph     string
		want      []string
		wantError string
	}{
		{name: "canonical", lexical: "arch/x86/boot/voffset.h", graph: "arch/x86/boot/voffset.h"},
		{
			name: "single traversal", lexical: "arch/x86/boot/compressed/../voffset.h", graph: "arch/x86/boot/voffset.h",
			want: []string{"arch/x86/boot/compressed"},
		},
		{
			name: "interleaved traversals", lexical: "arch/generated/../boot/compressed/../voffset.h", graph: "arch/boot/voffset.h",
			want: []string{"arch/boot/compressed", "arch/generated"},
		},
		{
			name: "nested traversals", lexical: "arch/x86/boot/compressed/nested/../../voffset.h", graph: "arch/x86/boot/voffset.h",
			want: []string{"arch/x86/boot/compressed", "arch/x86/boot/compressed/nested"},
		},
		{name: "escape", lexical: "../voffset.h", graph: "voffset.h", wantError: "escapes the working root"},
		{name: "wrong graph identity", lexical: "arch/x86/voffset.h", graph: "arch/arm64/voffset.h", wantError: "want graph target"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := compactKbuildLexicalTraversalWorkingDirectories(test.lexical, test.graph)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("directories = %q, error = %v, want error containing %q", got, err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("directories = %q, want %q", got, test.want)
			}
		})
	}
}

func TestConfiguredRustCommandUsesGenericRuleLowering(t *testing.T) {
	const (
		target = "tools/demo.o"
		source = "tools/demo.rs"
	)
	metadata := &CompactMetadata{
		actionRoles: testScopedActionRoles("rustc"),
	}

	for _, test := range []struct {
		name, stage, tree, product, scope, variable string
	}{
		{name: "target", stage: "target", tree: "objects", product: "vmlinux", scope: "target", variable: "RUSTC"},
		{name: "prehost", stage: "prehost", tree: "prehost", product: "sdk", scope: "host", variable: "HOSTRUSTC"},
		{name: "host", stage: "host", tree: "host", product: "sdk", scope: "host", variable: "HOSTRUSTC"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := mustCompactKbuildProfileForTest(t, "build:tools", "scripts/Makefile.build", "", `
cmd_rustc_o_rs = $(`+test.variable+`) --crate-type=proc-macro --emit=dep-info=tools/demo.d,obj=$@ $<
tools/demo.o: tools/demo.rs FORCE
	$(call if_changed,rustc_o_rs)
`, map[string]string{test.variable: KbuildActionRoleToken(test.scope, "rustc")})
			profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
			plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
			builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
			if test.stage != "target" {
				builder = builder.forOutput(test.stage, test.tree, test.product)
			}
			producer, err := builder.build(target)
			if err != nil {
				t.Fatal(err)
			}
			node, ok := compactKbuildPlanNode(plan, producer)
			if !ok {
				t.Fatalf("missing producer %q", producer)
			}
			recipe := plan.Recipes[node.Recipe]
			if node.Stage != test.stage || node.Product != test.product || node.Tool != "rustc" || node.Kind != "generate" {
				t.Fatalf("generic Rust node = %#v", node)
			}
			if got, want := node.Outputs, []ActionPlanOutput{{Tree: test.tree, Path: target}}; !slices.Equal(got, want) {
				t.Fatalf("outputs = %#v, want %#v", got, want)
			}
			if got, want := recipe.WorkingOutputs["00000000"], target; got != want {
				t.Fatalf("working output = %q, want %q", got, want)
			}
			if !slices.Contains(recipe.Arguments, "--crate-type=proc-macro") ||
				!slices.Contains(recipe.Arguments, "--emit=dep-info=tools/demo.d,obj="+target) {
				t.Fatalf("evaluated Rust arguments = %#v", recipe.Arguments)
			}
		})
	}
}

func TestConfiguredRustCommandProjectsCompilerOutputsToWritableObjectTree(t *testing.T) {
	const (
		target = "scripts/generate_rust_target"
		source = "scripts/generate_rust_target.rs"
	)
	profile := mustCompactKbuildProfileForTest(t, "host:scripts", "scripts/Makefile.host", "scripts", `
cmd_rustc_o_rs = $(HOSTRUSTC) --target=$(obj)/target.json --out-dir $(obj)/ --emit=dep-info=$(obj)/.generate_rust_target.d --emit=link=$@ $<
scripts/generate_rust_target: scripts/generate_rust_target.rs FORCE
	$(call if_changed,rustc_o_rs)
`, map[string]string{"HOSTRUSTC": KbuildActionRoleToken("host", "rustc")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	metadata := &CompactMetadata{
		actionRoles: testScopedActionRoles(append(testConfiguredActionRoles, "rustc")...),
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("host", "host", "sdk").
		forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing Rust host producer %q", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	for _, want := range []string{
		"${work:root}/scripts/",
		"--emit=dep-info=${work:root}/scripts/.generate_rust_target.d",
		"--emit=link=" + target,
	} {
		if !slices.Contains(recipe.Arguments, want) {
			t.Fatalf("Rust host arguments = %#v, want writable output argument %q", recipe.Arguments, want)
		}
	}
	for _, argument := range recipe.Arguments {
		if argument != "--target=${tree:prep}/scripts/target.json" && strings.Contains(argument, "${tree:prep}/scripts") {
			t.Fatalf("Rust host output argument retains immutable prep path: %q", argument)
		}
	}
	if !slices.Contains(recipe.Arguments, "--target=${tree:prep}/scripts/target.json") {
		t.Fatalf("Rust host arguments rewrote immutable target-spec input: %#v", recipe.Arguments)
	}
	if got, want := recipe.WorkingDirectories, []string{"scripts"}; !slices.Equal(got, want) {
		t.Fatalf("Rust host working directories = %#v, want %#v", got, want)
	}
}

func TestConfiguredCCompilerDeclaresWritableOutputParents(t *testing.T) {
	const (
		directory = "nested/invocation"
		target    = directory + "/demo.o"
		source    = directory + "/demo.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
cmd_cc_o_c = $(CC) -MF local/first.d -Wp,-MMD,local/second.d -Wp,-MD,../shared/third.d -c -o $@ $<
`+target+`: `+source+` FORCE
	$(call if_changed,cc_o_c)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing configured C producer %q", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if got, want := recipe.WorkingDirectories, []string{
		"nested/invocation", "nested/invocation/local", "nested/shared",
	}; !slices.Equal(got, want) {
		t.Fatalf("configured C working directories = %#v, want %#v", got, want)
	}
	for _, want := range []string{
		"${work:root}/nested/invocation/local/first.d",
		"-Wp,-MMD,${work:root}/nested/invocation/local/second.d",
		"-Wp,-MD,${work:root}/nested/shared/third.d",
	} {
		if !slices.Contains(recipe.Arguments, want) {
			t.Fatalf("configured C arguments = %#v, want %q", recipe.Arguments, want)
		}
	}
}

func TestRustCompilerMetadataFollowsEvaluatedLibrarySearchDependency(t *testing.T) {
	const (
		coreTarget     = "rust/core.o"
		metadataTarget = "rust/libcore.rmeta"
		consumerTarget = "rust/compiler_builtins.o"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:rust", "scripts/Makefile.build", "rust", `
objtree := .
obj := __LINUX_BZL_OBJECT_TREE__/rust
cmd_rust_core = $(RUSTC) --emit=obj=$@ --emit=metadata=rust/libcore.rmeta --crate-name core $<
cmd_post_core = $(OBJCOPY) --strip-debug $@
cmd_rust_consumer = $(RUSTC) --emit=dep-info=rust/.compiler_builtins.o.d --emit=obj=$@ -L$(objtree)/$(obj) --extern core --crate-name compiler_builtins $<
rust/core.o: rust/core.rs FORCE
	$(call if_changed,rust_core)
	$(call cmd,post_core)
rust/compiler_builtins.o: rust/compiler_builtins.rs rust/core.o FORCE
	$(call if_changed,rust_consumer)
`, map[string]string{
		"OBJCOPY": KbuildActionRoleToken("target", "objcopy"),
		"RUSTC":   KbuildActionRoleToken("target", "rustc"),
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "rust/core.rs", "rust/compiler_builtins.rs")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "rust",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testScopedActionRoles(append(testConfiguredActionRoles, "rustc")...),
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{
			"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity,
		},
		Recipes: map[string]ActionRecipe{},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile)
	producer, err := builder.build(consumerTarget)
	if err != nil {
		t.Fatal(err)
	}
	consumer, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing Rust consumer producer %q", producer)
	}
	consumerRecipe := plan.Recipes[consumer.Recipe]
	if !slices.Contains(consumerRecipe.Arguments, "-L${work:root}/rust") {
		t.Fatalf("Rust consumer arguments = %#v, want canonical object-tree library search", consumerRecipe.Arguments)
	}
	if !slices.Contains(consumerRecipe.Arguments, "--extern") || !slices.Contains(consumerRecipe.Arguments, "core") {
		t.Fatalf("Rust consumer arguments = %#v, want source-selected implicit core dependency", consumerRecipe.Arguments)
	}

	var core ActionPlanNode
	metadataSlot := -1
	for _, node := range plan.Nodes {
		for slot, output := range node.Outputs {
			if output.Path == metadataTarget {
				core = node
				metadataSlot = slot
				if !output.persistent {
					t.Fatalf("Rust core metadata output is not marked persistent: %#v", output)
				}
			}
		}
	}
	if core.ID == "" || metadataSlot < 0 {
		t.Fatalf("Rust core action omits persistent metadata output %q: %#v", metadataTarget, plan.Nodes)
	}
	coreRecipe := plan.Recipes[core.Recipe]
	if got := coreRecipe.WorkingOutputs[planOrdinal(metadataSlot)]; got != metadataTarget {
		t.Fatalf("Rust core metadata working output = %q, want %q", got, metadataTarget)
	}
	if !slices.ContainsFunc(core.Outputs, func(output ActionPlanOutput) bool { return output.Path == coreTarget }) {
		t.Fatalf("Rust core action lost primary object output: %#v", core.Outputs)
	}
	for _, node := range plan.Nodes {
		for _, output := range node.Outputs {
			if output.Path == "rust/.core.o.d" || output.Path == "rust/.compiler_builtins.o.d" {
				t.Fatalf("Rust dep-info scratch output was published: %#v", output)
			}
		}
	}
	directCoreObject := false
	for _, input := range consumer.Inputs {
		producer, ok := compactKbuildPlanNode(plan, input.ProducerID)
		if ok && input.Role == "prerequisite" && input.Slot >= 0 && input.Slot < len(producer.Outputs) &&
			producer.Outputs[input.Slot].Path == coreTarget {
			directCoreObject = true
		}
	}
	if !directCoreObject {
		t.Fatalf("Rust consumer inputs = %#v, want direct %q prerequisite", consumer.Inputs, coreTarget)
	}
	if got, want := len(actionPlanNodeInputSetEntriesForTest(t, plan, consumer)), 1; got != want {
		t.Fatalf("Rust consumer persistent inputs = %#v, want metadata only", actionPlanNodeInputSetEntriesForTest(t, plan, consumer))
	}
	metadataInput, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, consumer, metadataTarget)
	wantMetadataInput := ActionPlanInputSetEntry{
		Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: metadataTarget},
		ProducerID: core.ID,
		Slot:       metadataSlot,
	}
	if !found || metadataInput != wantMetadataInput {
		t.Fatalf("Rust consumer persistent metadata input = (%#v, %t), want %#v", metadataInput, found, wantMetadataInput)
	}
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("Rust metadata action plan: %v", err)
	}
}

func TestCompoundCompilerOutputsAggregateEveryTypedCompilerRole(t *testing.T) {
	const directory = "nested/invocation"
	profile := CompactKbuildProfile{}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	template := strings.Join([]string{
		KbuildActionRoleToken("target", "cc") + " -I__LINUX_BZL_OBJECT_TREE__/arch/x86/include/generated -MF cc/first.d -o cc/first.o input.c",
		KbuildActionRoleToken("host", "cxx") + " -imacros __LINUX_BZL_OBJECT_TREE__/include/generated/rustc_cfg -Wp,-MMD,cxx/second.d -o cxx/second.o input.cc",
		KbuildActionRoleToken("host", "rustc") + " --out-dir rust/test --emit=dep-info=rust/deps/test.d,link=rust/test/test -L rust/lib input.rs",
		KbuildActionRoleToken("target", "clippy") + " --out-dir=clippy/test --emit=dep-info=clippy/deps/test.d,metadata=clippy/meta/test.rmeta -Ldependency=clippy/lib input.rs",
	}, "; ")
	commands, err := compactKbuildCompoundProgramCommands(template)
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := compactKbuildCompoundCompilerOutputs(profile, nil, nil, "", compactKbuildRuleMatch{}, nil, "", template, commands)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := analysis.WorkingDirectories, []string{
		"nested/invocation/cc",
		"nested/invocation/clippy/deps",
		"nested/invocation/clippy/meta",
		"nested/invocation/clippy/test",
		"nested/invocation/cxx",
		"nested/invocation/rust/deps",
		"nested/invocation/rust/test",
	}; !slices.Equal(got, want) {
		t.Fatalf("compound compiler working directories = %#v, want %#v", got, want)
	}
	if got, want := analysis.PersistentOutputs, []string{
		"nested/invocation/clippy/meta/test.rmeta",
		"nested/invocation/rust/test/test",
	}; !slices.Equal(got, want) {
		t.Fatalf("compound compiler persistent outputs = %#v, want %#v", got, want)
	}
	if got, want := analysis.LibrarySearchDirectories, []string{
		"nested/invocation/clippy/lib",
		"nested/invocation/rust/lib",
	}; !slices.Equal(got, want) {
		t.Fatalf("compound compiler library directories = %#v, want %#v", got, want)
	}
	if got, want := analysis.PreparedObjectDirectories, []string{"arch/x86/include/generated"}; !slices.Equal(got, want) {
		t.Fatalf("compound compiler prepared directories = %#v, want %#v", got, want)
	}
	if got, want := analysis.PreparedObjectIncludeFiles, []string{"include/generated/rustc_cfg"}; !slices.Equal(got, want) {
		t.Fatalf("compound compiler prepared inputs = %#v, want %#v", got, want)
	}
	rewritten, err := applyCompactKbuildScriptSourceReplacements(template, analysis.Replacements)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"'" + compactKbuildActionObjectTreeMarker + "/nested/invocation/cc/first.d'",
		"'-Wp,-MMD," + compactKbuildActionObjectTreeMarker + "/nested/invocation/cxx/second.d'",
		"'--out-dir=" + compactKbuildActionObjectTreeMarker + "/nested/invocation/clippy/test'",
		"'--emit=dep-info=" + compactKbuildActionObjectTreeMarker + "/nested/invocation/rust/deps/test.d,link=" + compactKbuildActionObjectTreeMarker + "/nested/invocation/rust/test/test'",
		"'" + compactKbuildActionObjectTreeMarker + "/nested/invocation/rust/lib'",
		"'-Ldependency=" + compactKbuildActionObjectTreeMarker + "/nested/invocation/clippy/lib'",
	} {
		if !strings.Contains(rewritten, want) {
			t.Errorf("rewritten compound compiler script = %q, want %q", rewritten, want)
		}
	}
}

func TestCompoundCompilerOutputsProjectGeneratedInputFromTypedInvocation(t *testing.T) {
	const (
		directory = ".linux-bzl/external/demo"
		input     = directory + "/demo.mod.c"
	)
	profile := CompactKbuildProfile{}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	template := KbuildActionRoleToken("target", "cc") + " -c -o demo.mod.o demo.mod.c"
	commands, err := compactKbuildCompoundProgramCommands(template)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		input compactKbuildRuleInput
	}{
		{
			name:  "producer-backed",
			input: compactKbuildRuleInput{path: input, producer: "modpost", slot: 0},
		},
		{
			name: "configured-object-tree",
			input: compactKbuildRuleInput{
				path: input, sourceID: "configured-object-tree", objectTree: true, workingOnly: true,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			analysis, err := compactKbuildCompoundCompilerOutputs(
				profile,
				nil,
				nil,
				strings.TrimSuffix(input, ".c")+".o",
				compactKbuildRuleMatch{},
				[]compactKbuildRuleInput{test.input},
				"../../..",
				template,
				commands,
			)
			if err != nil {
				t.Fatal(err)
			}
			rewritten, err := applyCompactKbuildScriptSourceReplacements(template, analysis.Replacements)
			if err != nil {
				t.Fatal(err)
			}
			if want := "'../../../" + input + "'"; !strings.Contains(rewritten, want) {
				t.Fatalf("rewritten compound compiler script = %q, want typed generated input %q", rewritten, want)
			}
			if unprojected := "'" + input + "'"; strings.Contains(rewritten, unprojected) {
				t.Fatalf("rewritten compound compiler script = %q, contains unprojected generated input %q", rewritten, unprojected)
			}
		})
	}
}

func TestHermeticCompilerScriptsProjectRustOutDirAndDepfile(t *testing.T) {
	const (
		directory      = "nested/invocation"
		target         = directory + "/rusttest-macros"
		metadataTarget = directory + "/libcore.rmeta"
	)
	profile := mustCompactKbuildProfileForTest(t, "rust:"+directory, "rust/Makefile", directory, `
`+target+`: FORCE
	@true
`, nil)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	role := KbuildActionRoleToken("target", "rustc")
	outputRoot := compactKbuildActionObjectTreeMarker + "/" + directory
	compiler := role +
		" --test --out-dir " + outputRoot + "/test" +
		" --emit=dep-info=" + outputRoot + "/deps/rusttest-macros.d,link=" + outputRoot + "/rusttest-macros,metadata=" + outputRoot + "/libcore.rmeta"
	for _, test := range []struct {
		name     string
		template string
		wantRoot string
		build    func(compactKbuildRulePlanBuilder, compactKbuildRuleMatch, string, []compactKbuildRecipeCommand) (string, error)
	}{
		{
			name:     "direct hermetic script",
			template: compiler,
			wantRoot: "${tree:prep}",
			build: func(builder compactKbuildRulePlanBuilder, match compactKbuildRuleMatch, template string, _ []compactKbuildRecipeCommand) (string, error) {
				return builder.buildHermeticKbuildScript(target, match, nil, template)
			},
		},
		{
			name:     "atomic statement sequence",
			template: compiler + "; true",
			wantRoot: "../..",
			build: func(builder compactKbuildRulePlanBuilder, match compactKbuildRuleMatch, template string, commands []compactKbuildRecipeCommand) (string, error) {
				return builder.buildHermeticKbuildCompound(target, match, nil, template, commands)
			},
		},
		{
			name:     "pipeline",
			template: compiler + " | cat > " + outputRoot + "/rusttest-macros",
			wantRoot: "../..",
			build: func(builder compactKbuildRulePlanBuilder, match compactKbuildRuleMatch, template string, commands []compactKbuildRecipeCommand) (string, error) {
				return builder.buildHermeticKbuildPipeline(target, match, nil, template, commands)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			commands, err := compactKbuildCompoundProgramCommands(test.template)
			if err != nil {
				t.Fatal(err)
			}
			metadata := &CompactMetadata{
				actionRoles: testScopedActionRoles(append(testConfiguredActionRoles, "rustc")...),
			}
			plan := &ActionPlan{
				Toolsets: map[string]string{
					"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity,
				},
				Recipes: map[string]ActionRecipe{},
			}
			builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
			producer, err := test.build(builder, compactKbuildRuleMatch{
				profile: profile,
				rule:    profile.Rules[0],
			}, test.template, commands)
			if err != nil {
				t.Fatal(err)
			}
			node, ok := compactKbuildPlanNode(plan, producer)
			if !ok {
				t.Fatalf("missing hermetic Rust producer %q", producer)
			}
			recipe := plan.Recipes[node.Recipe]
			if got, want := recipe.WorkingDirectories, []string{
				"nested/invocation",
				"nested/invocation/deps",
				"nested/invocation/test",
			}; !slices.Equal(got, want) {
				t.Fatalf("Rust script working directories = %#v, want %#v", got, want)
			}
			script := compactKbuildRecipeScriptContentForTest(t, recipe)
			for _, want := range []string{
				"'" + test.wantRoot + "/nested/invocation/test'",
				"'--emit=dep-info=" + test.wantRoot + "/nested/invocation/deps/rusttest-macros.d,link=" + test.wantRoot + "/nested/invocation/rusttest-macros,metadata=" + test.wantRoot + "/nested/invocation/libcore.rmeta'",
			} {
				if !strings.Contains(script, want) {
					t.Errorf("Rust script = %q, want projected compiler argument %q", script, want)
				}
			}
			if strings.Contains(script, "${work:root}") || strings.Contains(script, compactKbuildActionObjectTreeMarker) {
				t.Fatalf("Rust script retains an unlowered writable-object placeholder: %q", script)
			}
			if got := slices.Contains(recipe.Arguments, "prep=${work:root}"); got != (test.wantRoot == "${tree:prep}") {
				t.Fatalf("Rust script prep binding = %t, want %t; arguments=%q", got, test.wantRoot == "${tree:prep}", recipe.Arguments)
			}
			targetSlot := -1
			metadataSlot := -1
			for slot, output := range node.Outputs {
				if output.Path == target {
					targetSlot = slot
					if !output.persistent {
						t.Fatalf("Rust link output is not marked persistent: %#v", output)
					}
				}
				if output.Path != metadataTarget {
					continue
				}
				metadataSlot = slot
				if output.ArtifactPath == "" || output.ArtifactPath == metadataTarget {
					t.Fatalf("Rust metadata output is not physically private: %#v", output)
				}
				if !output.persistent {
					t.Fatalf("Rust metadata output is not marked persistent: %#v", output)
				}
			}
			if metadataSlot < 0 {
				t.Fatalf("Rust script omits persistent metadata output %q: %#v", metadataTarget, node.Outputs)
			}
			if targetSlot < 0 {
				t.Fatalf("Rust script omits persistent link output %q: %#v", target, node.Outputs)
			}
			if got := recipe.WorkingOutputs[planOrdinal(metadataSlot)]; got != metadataTarget {
				t.Fatalf("Rust metadata working output = %q, want %q", got, metadataTarget)
			}
			if slices.ContainsFunc(node.Outputs, func(output ActionPlanOutput) bool {
				return output.Path == directory+"/deps/rusttest-macros.d"
			}) {
				t.Fatalf("Rust dep-info scratch was published: %#v", node.Outputs)
			}
			if _, err := plan.entries(); err != nil {
				t.Fatalf("Rust script action plan: %v", err)
			}
		})
	}
}

func TestPersistentCompilerOutputRejectsObservedStateAlias(t *testing.T) {
	_, err := compactKbuildPersistentCompilerOutputs(
		[]ActionPlanOutput{{
			Tree: "prep", Path: "rust/libkernel.rmeta",
			ObservedPath: "rust/libkernel.rmeta",
		}},
		[]string{"rust/libkernel.rmeta"},
		"prep",
		func(string) string { return ".linux-bzl-intermediate/unused.rmeta" },
	)
	if err == nil || !strings.Contains(err.Error(), "aliases observed-state capture") {
		t.Fatalf("persistent observed-state alias error = %v", err)
	}
}

func TestHermeticCppPipelineLowersDependencyOutputBeforeEncoding(t *testing.T) {
	const target = "rust/kernel/generated_arch_warn_asm.rs"
	profile := mustCompactKbuildProfileForTest(t, "rust:kernel", "scripts/Makefile.build", "", `
`+target+`: FORCE
	@true
`, nil)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	template := KbuildActionRoleToken("target", "cc") +
		" -E -Wp,-MMD," + compactKbuildActionObjectTreeMarker + "/rust/kernel/.generated_arch_warn_asm.rs.d" +
		" " + compactKbuildActionSourceTreeMarker + "/rust/kernel/generated_arch_warn_asm.rs.S" +
		" | sed '1,/^\\/\\/ Cut here.$/d' >" + target
	commands, err := compactKbuildCompoundProgramCommands(template)
	if err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{actionRoles: testConfiguredScopedActionRoles}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildHermeticKbuildPipeline(target, compactKbuildRuleMatch{
		profile: profile,
		rule:    profile.Rules[0],
	}, nil, template, commands)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing CPP pipeline producer %q", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if got, want := recipe.WorkingDirectories, []string{"rust/kernel"}; !slices.Equal(got, want) {
		t.Fatalf("CPP pipeline working directories = %#v, want %#v", got, want)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if !strings.Contains(script, "'-Wp,-MMD,rust/kernel/.generated_arch_warn_asm.rs.d'") {
		t.Fatalf("CPP pipeline script does not contain its cwd-relative dependency output: %q", script)
	}
	if strings.Contains(script, "${work:root}") || strings.Contains(script, compactKbuildActionObjectTreeMarker) {
		t.Fatalf("CPP pipeline script retains an unlowered writable-object placeholder: %q", script)
	}
}

func TestConfiguredCommandBindsOppositeScopeAuxiliaryTools(t *testing.T) {
	for _, test := range []struct {
		name, command, stage, tree, product, tool, auxiliary, argument string
		variables                                                      map[string]string
	}{
		{
			name:    "target rustc with host linker",
			command: `$(RUSTC) --crate-type proc-macro -Clinker-flavor=gcc -Clinker=$(HOSTCC) -o $@ $<`,
			variables: map[string]string{
				"RUSTC":  KbuildActionRoleToken("target", "rustc"),
				"HOSTCC": KbuildActionRoleToken("host", "cc"),
			},
			stage: "target", tree: "objects", product: "vmlinux", tool: "rustc",
			auxiliary: "host@cc", argument: "-Clinker=${tool:host@cc}",
		},
		{
			name:    "host cc with target cc auxiliary",
			command: `$(HOSTCC) --target-driver=$(CC) -o $@ $<`,
			variables: map[string]string{
				"HOSTCC": KbuildActionRoleToken("host", "cc"),
				"CC":     KbuildActionRoleToken("target", "cc"),
			},
			stage: "host", tree: "host", product: "sdk", tool: "cc",
			auxiliary: "target@cc", argument: "--target-driver=${tool:target@cc}",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			const target = "tools/result"
			const source = "tools/input.rs"
			profile := mustCompactKbuildProfileForTest(t, "build:tools", "scripts/Makefile.build", "", `
cmd_build = `+test.command+`
tools/result: tools/input.rs FORCE
	$(call if_changed,build)
`, test.variables)
			profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
			metadata := &CompactMetadata{actionRoles: []KbuildActionRoleRef{
				{Scope: "host", Role: "cc"},
				{Scope: "target", Role: "cc"},
				{Scope: "target", Role: "rustc"},
			}}
			plan := &ActionPlan{
				Toolsets: map[string]string{
					"host": actionPlanTestProbeIdentity, "target": actionPlanTestProbeIdentity,
				},
				Recipes: map[string]ActionRecipe{},
			}
			builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
			if test.stage != "target" {
				builder = builder.forOutput(test.stage, test.tree, test.product)
			}
			producer, err := builder.build(target)
			if err != nil {
				t.Fatal(err)
			}
			node, ok := compactKbuildPlanNode(plan, producer)
			if !ok {
				t.Fatalf("missing producer %q", producer)
			}
			recipe := plan.Recipes[node.Recipe]
			if node.Stage != test.stage || node.Tool != test.tool ||
				!slices.Equal(node.AuxiliaryTools, []string{test.auxiliary}) ||
				!slices.Equal(recipe.AuxiliaryTools, []string{test.auxiliary}) {
				t.Fatalf("mixed-scope node=%#v recipe auxiliary=%q", node, recipe.AuxiliaryTools)
			}
			if !slices.Contains(recipe.Arguments, test.argument) {
				t.Fatalf("arguments = %#v, want scoped binding %q", recipe.Arguments, test.argument)
			}
			for _, argument := range recipe.Arguments {
				if strings.Contains(argument, kbuildActionRoleTokenPrefix) {
					t.Fatalf("argument retains source action-role token: %q", argument)
				}
			}
			if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
				t.Fatalf("write mixed-scope plan: %v", err)
			}
		})
	}
}

func TestNormalizeKbuildRecipePreservesPrivateDepfileCleanup(t *testing.T) {
	commands := []compactKbuildRecipeCommand{
		{program: KbuildActionRoleToken("host", "cc"), arguments: []string{"-Wp,-MMD,${tree:prep}/scripts/basic/.fixdep.d", "-o", "scripts/basic/fixdep", "scripts/basic/fixdep.c"}, connector: ";"},
		{program: "${tree:prep}/scripts/basic/fixdep", arguments: []string{"${tree:prep}/scripts/basic/.fixdep.d", "scripts/basic/fixdep"}, connector: ";"},
		{program: "rm", arguments: []string{"-f", "${tree:prep}/scripts/basic/.fixdep.d"}},
	}
	normalized, err := normalizeCompactKbuildRecipeCommands("scripts/basic/fixdep", commands)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(normalized, commands, func(left, right compactKbuildRecipeCommand) bool {
		return left.program == right.program && slices.Equal(left.arguments, right.arguments)
	}) {
		t.Fatalf("normalized cmd_and_fixdep = %#v, want cleanup preserved", normalized)
	}

	stale := append([]compactKbuildRecipeCommand{{program: "rm", arguments: []string{"-f", "scripts/basic/fixdep"}, connector: ";"}}, commands...)
	normalized, err = normalizeCompactKbuildRecipeCommands("scripts/basic/fixdep", stale)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != len(commands) {
		t.Fatalf("normalized stale-output prefix has %d commands, want %d", len(normalized), len(commands))
	}

	multiCleanup := slices.Clone(commands)
	multiCleanup[len(multiCleanup)-1].arguments = []string{
		"-f",
		"${tree:prep}/scripts/basic/.fixdep.d",
		"${tree:prep}/scripts/basic/.fixdep.tmp",
	}
	normalized, err = normalizeCompactKbuildRecipeCommands("scripts/basic/fixdep", multiCleanup)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(normalized[len(normalized)-1].arguments, multiCleanup[len(multiCleanup)-1].arguments) {
		t.Fatalf("normalized multi-path cleanup = %q, want %q", normalized[len(normalized)-1].arguments, multiCleanup[len(multiCleanup)-1].arguments)
	}

	multiPrefix := append([]compactKbuildRecipeCommand{{
		program: "rm", arguments: []string{"-f", "scripts/basic/fixdep", "scripts/basic/.fixdep.tmp"}, connector: ";",
	}}, commands...)
	normalized, err = normalizeCompactKbuildRecipeCommands("scripts/basic/fixdep", multiPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != len(multiPrefix) || normalized[0].program != "rm" {
		t.Fatalf("multi-path target cleanup was incorrectly erased: %#v", normalized)
	}
}

func TestParseCompactKbuildRecipePreservesShellNewlineBoundaries(t *testing.T) {
	commands, err := parseCompactKbuildRecipe(
		"rm -f scripts/basic/.fixdep.d\n:\n",
		compactKbuildAutomaticContext{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[0].program != "rm" ||
		!slices.Equal(commands[0].arguments, []string{"-f", "scripts/basic/.fixdep.d"}) ||
		commands[0].connector != ";" || commands[1].program != ":" {
		t.Fatalf("newline-separated recipe = %#v, want bounded rm followed by no-op", commands)
	}

	commands, err = parseCompactKbuildRecipe(
		"tool first \\\nsecond",
		compactKbuildAutomaticContext{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].program != "tool" ||
		!slices.Equal(commands[0].arguments, []string{"first", "second"}) {
		t.Fatalf("continued recipe = %#v, want one argv", commands)
	}
}

func TestKbuildOutputlessCommandAndSavecmdTailStaysAtomic(t *testing.T) {
	const target = "generated/result.h"
	commands := []compactKbuildRecipeCommand{
		{program: "set", arguments: []string{"-e"}, connector: ";"},
		{
			program: "sh",
			arguments: []string{
				"${tree:kernel}/scripts/transform.sh",
				target,
				"input.txt",
			},
			connector: ";",
		},
		{
			program: "printf", arguments: []string{"%s\\n", "savedcmd_" + target},
			stdout: ".result.h.cmd",
		},
	}
	atomic, err := compactKbuildRecipeRequiresAtomicExecution(
		target,
		compactKbuildRuleMatch{},
		commands,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !atomic {
		t.Fatal("outputless source command followed by savecmd side output was split across actions")
	}
}

func TestKbuildConfiguredRustFixdepWrapperRequiresAtomicExecution(t *testing.T) {
	const (
		target  = "scripts/generate_rust_target"
		depfile = "scripts/.generate_rust_target.d"
	)
	commands := []compactKbuildRecipeCommand{
		{program: "set", arguments: []string{"-e"}, connector: ";"},
		{
			program: KbuildActionRoleToken("host", "rustc"),
			arguments: []string{
				"--out-dir", "scripts/",
				"--emit=dep-info=" + depfile,
				"--emit=link=" + target,
				"scripts/generate_rust_target.rs",
			},
			connector: ";",
		},
		{
			program:   "__LINUX_BZL_OBJECT_TREE__/scripts/basic/fixdep",
			arguments: []string{depfile, target, "saved command"},
			stdout:    "scripts/.generate_rust_target.cmd",
			connector: ";",
		},
		{program: "rm", arguments: []string{"-f", depfile}},
	}
	atomic, err := compactKbuildRecipeRequiresAtomicExecution(
		target,
		compactKbuildRuleMatch{},
		commands,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !atomic {
		t.Fatal("source-selected Rust/fixdep wrapper was split across actions")
	}
}

func TestKbuildCompoundSurvivingOutputsPreserveRootedExternalPaths(t *testing.T) {
	const directory = ".linux-bzl/external/example"
	profile := CompactKbuildProfile{}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	root := "${tree:prep}/" + directory
	commands := []compactKbuildRecipeCommand{
		{
			program: KbuildActionRoleToken("target", "cc"),
			arguments: []string{
				"-Wp,-MMD," + root + "/.module.o.d",
				"-c", "-o", root + "/module.o", "${tree:kernel}/module.c",
			},
			connector: ";",
		},
		{
			program: root + "/scripts/basic/fixdep",
			arguments: []string{
				root + "/.module.o.d", root + "/module.o", "saved command",
			},
			stdout: root + "/.module.o.cmd", connector: ";",
		},
		{program: "rm", arguments: []string{"-f", root + "/.module.o.d"}},
	}
	outputs, err := compactKbuildCompoundSurvivingExplicitOutputs(profile, commands, directory+"/module.o")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{directory + "/.module.o.cmd"}; !slices.Equal(outputs, want) {
		t.Fatalf("surviving rooted external outputs=%q, want %q", outputs, want)
	}

	commands[1].stdout = ".module.o.cmd"
	commands[2].arguments[1] = ".module.o.d"
	outputs, err = compactKbuildCompoundSurvivingExplicitOutputs(profile, commands, directory+"/module.o")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{directory + "/.module.o.cmd"}; !slices.Equal(outputs, want) {
		t.Fatalf("surviving invocation-relative external outputs=%q, want %q", outputs, want)
	}

	discovered, err := compactKbuildCompoundProgramCommands("false && printf data > state.cmd")
	if err != nil {
		t.Fatal(err)
	}
	outputs, err = compactKbuildCompoundSurvivingExplicitOutputs(profile, discovered)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 0 {
		t.Fatalf("lossy control-flow discovery declared outputs: %q", outputs)
	}

	multiRemoval := []compactKbuildRecipeCommand{
		{program: "printf", arguments: []string{"one"}, stdout: "state/one.tmp", connector: ";"},
		{program: "printf", arguments: []string{"two"}, stdout: "state/two.tmp", connector: ";"},
		{program: "rm", arguments: []string{"-f", "state/one.tmp", "state/two.tmp"}},
	}
	outputs, err = compactKbuildCompoundSurvivingExplicitOutputs(profile, multiRemoval)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 0 {
		t.Fatalf("multi-path cleanup retained removed outputs: %q", outputs)
	}

	multiRemoval[2].arguments[2] = "state/*.tmp"
	if _, err := compactKbuildCompoundSurvivingExplicitOutputs(profile, multiRemoval); err == nil ||
		!strings.Contains(err.Error(), "dynamically expanded path") {
		t.Fatalf("glob cleanup error = %v, want conservative rejection", err)
	}
}

func TestGenericKbuildLinkerScriptExpandsOutOfTreeIncludeFlags(t *testing.T) {
	const (
		directory = "arch/x86/kernel"
		target    = directory + "/vmlinux.lds"
		source    = target + ".S"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
_cpp_flags = $(KBUILD_CPPFLAGS) $(CPPFLAGS_$(target-stem).lds)
ifdef building_out_of_srctree
_cpp_flags += $(addprefix -I, $(src) $(obj))
endif
cmd_cpp_lds_S = $(CC) $(_cpp_flags) -o $@ $<
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"), "building_out_of_srctree": "1",
		"KBUILD_CPPFLAGS":      "-fmacro-prefix-map=__LINUX_BZL_SOURCE_TREE__/=",
		"CPPFLAGS_vmlinux.lds": "-P -Ulinux -D__ASSEMBLY__ -DLINKER_SCRIPT",
		"obj":                  directory, "src": "__LINUX_BZL_SOURCE_TREE__/" + directory,
	})
	metadata := &CompactMetadata{actionRoles: testConfiguredScopedActionRoles}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "vmlinux", command: "cpp_lds_S",
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("linker-script producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	for _, want := range []string{
		"-fmacro-prefix-map=${tree:kernel}/=",
		"-I${tree:kernel}/" + directory,
		"-I${tree:prep}/" + directory,
		"-P", "-Ulinux", "-D__ASSEMBLY__", "-DLINKER_SCRIPT",
	} {
		if !slices.Contains(recipe.Arguments, want) {
			t.Fatalf("linker-script arguments = %#v, want %q", recipe.Arguments, want)
		}
	}
	if joined := strings.Join(recipe.Arguments, " "); strings.Contains(joined, "$(addprefix") || strings.Contains(joined, "__LINUX_BZL_") {
		t.Fatalf("linker-script arguments retain Make/tree markers: %q", joined)
	}
}

func TestGenericKbuildExplicitCompositeHostToolBindsGeneratedObjects(t *testing.T) {
	const target = "arch/x86/tools/relocs"
	objects := []string{
		"arch/x86/tools/relocs_32.o",
		"arch/x86/tools/relocs_64.o",
		"arch/x86/tools/relocs_common.o",
	}
	profile := mustCompactKbuildProfileForTest(t, "host:relocs", "scripts/Makefile.host", "arch/x86/tools", `
target-stem = $(basename $(patsubst $(obj)/%,%,$@))
relocs-objs := relocs_32.o relocs_64.o relocs_common.o
cmd_host-cmulti = $(HOSTCC) -o $@ $(addprefix $(obj)/,$($(target-stem)-objs))
`, map[string]string{
		"HOSTCC": KbuildActionRoleToken("host", "cc"),
		"obj":    "arch/x86/tools",
	})
	metadata := &CompactMetadata{actionRoles: testConfiguredScopedActionRoles}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	inputs := make([]compactKbuildRuleInput, 0, len(objects))
	for _, object := range objects {
		node := ActionPlanNode{
			Stage: "host", Kind: "compile", Tool: "cc", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: "host", Path: object}},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
			Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
		}
		producer, err := appendActionPlanNode(plan, node, recipe)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, compactKbuildRuleInput{path: object, producer: producer})
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("host", "host", "sdk").
		forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, command: "host-cmulti",
	}, inputs)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("relocs producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Kind != "link-driver" || node.Tool != "cc" || recipe.Kind != "link-driver" || recipe.Tool != "cc" {
		t.Fatalf("relocs action = node %#v recipe %#v, want source-selected cc with link-driver contract", node, recipe)
	}
	want := []string{
		"-o", "${output:00000000}",
		"${input:prerequisite:00000000}",
		"${input:prerequisite:00000001}",
		"${input:prerequisite:00000002}",
	}
	if !slices.Equal(recipe.Arguments, want) {
		t.Fatalf("relocs arguments = %#v, want %#v", recipe.Arguments, want)
	}
	if len(node.Inputs) != len(objects) {
		t.Fatalf("relocs inputs = %#v, want %d generated objects", node.Inputs, len(objects))
	}
}

func TestGenericKbuildHostRelocatableFilterRetainsObjectTreeAutomaticPrerequisites(t *testing.T) {
	const (
		directory = "tools/objtool/arch/x86"
		target    = directory + "/objtool-in.o"
	)
	objects := []string{
		directory + "/decode.o",
		directory + "/orc.o",
		directory + "/special.o",
	}
	profile := mustCompactKbuildProfileForTest(t, "host:objtool-x86", "tools/build/Makefile.build", directory, `
obj-y := $(addprefix $(obj)/,decode.o orc.o special.o)
cmd_host_ld_multi = $(if $(strip $(obj-y)),$(HOSTLD) -r -o $@ $(filter $(obj-y),$^),false)
$(obj)/objtool-in.o: $(addprefix $(obj)/,decode.o orc.o special.o) FORCE
	$(call if_changed,host_ld_multi)
`, map[string]string{
		"HOSTLD": KbuildActionRoleToken("host", "ld"),
		"obj":    "__LINUX_BZL_OBJECT_TREE__/" + directory,
	})
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	for _, object := range objects {
		node := ActionPlanNode{
			Stage: "host", Kind: "compile", Tool: "cc", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: "host", Path: object}},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
			Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
		}
		if _, err := appendActionPlanNode(plan, node, recipe); err != nil {
			t.Fatal(err)
		}
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("host", "host", "sdk").
		forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("objtool aggregate producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	want := []string{
		"-r", "-o", "${output:00000000}",
		"${input:prerequisite:00000000}",
		"${input:prerequisite:00000001}",
		"${input:prerequisite:00000002}",
	}
	if !slices.Equal(recipe.Arguments, want) {
		t.Fatalf("objtool aggregate arguments = %#v, want %#v", recipe.Arguments, want)
	}
	if node.Kind != "link-relocatable" || node.Tool != "ld" || len(node.Inputs) != len(objects) {
		t.Fatalf("objtool aggregate action = node %#v recipe %#v", node, recipe)
	}
}

func TestGenericKbuildObjectRootTargetBindsOutputInsideWritableObjectTree(t *testing.T) {
	const (
		directory = "tools/objtool/libsubcmd"
		target    = directory + "/exec-cmd.o"
		source    = directory + "/exec-cmd.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:libsubcmd", "tools/build/Makefile.build", directory, `
cmd_cc_o_c = $(CC) -c -o $@ $<
$(OUTPUT)%.o: %.c FORCE
	$(call if_changed,cc_o_c)
`, map[string]string{
		"CC":     KbuildActionRoleToken("host", "cc"),
		"OUTPUT": "__LINUX_BZL_OBJECT_TREE__/" + directory + "/",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("host", "host", "sdk").
		withInitialObjectTree(true).
		forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("compile producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	want := []string{"-c", "-o", "${output:00000000}", "${source:object:00000000}"}
	if !slices.Equal(recipe.Arguments, want) {
		t.Fatalf("writable object-tree compile arguments = %#v, want %#v", recipe.Arguments, want)
	}
	if len(node.Outputs) != 1 || node.Outputs[0].Path != target || !slices.Equal(recipe.Outputs, []string{"00000000"}) {
		t.Fatalf("writable object-tree compile outputs = node %#v recipe %#v", node.Outputs, recipe.Outputs)
	}
	if got := recipe.WorkingOutputs["00000000"]; got != target {
		t.Fatalf("writable object-tree compile working output = %q, want %q", got, target)
	}
}

func TestGenericKbuildSplitSourceAndObjectRootsPreserveAutomaticTarget(t *testing.T) {
	const (
		target = "tools/objtool/libsubcmd/exec-cmd.o"
		source = "tools/lib/subcmd/exec-cmd.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:libsubcmd", "tools/build/Makefile.build", "libsubcmd", `
quiet_cmd_mkdir = MKDIR $(dir $@)
cmd_mkdir = mkdir -p $(dir $@)
rule_mkdir = $(if $(wildcard $(dir $@)),,@$(cmd_mkdir))
if_changed_dep = $(cmd_$(1))
cmd_cc_o_c = $(CC) -c -o $@ $<
$(OUTPUT)%.o: %.c FORCE
	$(call rule_mkdir)
	$(call if_changed_dep,cc_o_c)
`, map[string]string{
		"CC":     KbuildActionRoleToken("host", "cc"),
		"OUTPUT": "__LINUX_BZL_OBJECT_TREE__/tools/objtool/libsubcmd/",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: "tools/lib/subcmd",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("host", "host", "sdk").
		forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok || node.Tool != "cc" || len(node.Outputs) == 0 || node.Outputs[0].Path != target {
		t.Fatalf("split-root compile node = %#v, want host cc output %q", node, target)
	}
	recipe := plan.Recipes[node.Recipe]
	if !slices.Contains(recipe.WorkingDirectories, "tools/objtool/libsubcmd") {
		t.Fatalf("split-root compiler directories = %q, want object output parent", recipe.WorkingDirectories)
	}
	for _, directory := range recipe.WorkingDirectories {
		if strings.Contains(directory, "tools/lib/subcmd/tools/objtool") {
			t.Fatalf("split-root compiler output was scoped below source cwd: %q", recipe.WorkingDirectories)
		}
	}
}

func TestGenericKbuildSplitSourceAndObjectRootsCompoundCompilerPreservesSourceProvenance(t *testing.T) {
	const (
		target = "tools/objtool/libsubcmd/subcmd-config.o"
		source = "tools/lib/subcmd/subcmd-config.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:libsubcmd", "tools/build/Makefile.build", "libsubcmd", `
cmd_cc_o_c = $(CC) -c -o $@ $<; printf saved > $(OUTPUT).subcmd-config.o.cmd
$(OUTPUT)%.o: %.c FORCE
	$(call if_changed_dep,cc_o_c)
`, map[string]string{
		"CC":     KbuildActionRoleToken("host", "cc"),
		"OUTPUT": "__LINUX_BZL_OBJECT_TREE__/tools/objtool/libsubcmd/",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: "tools/lib/subcmd",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("host", "host", "sdk").
		forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("split-root compound producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("split-root compound node = %#v recipe = %#v, want scriptrun", node, recipe)
	}
	if recipe.ExecutionDirectory != "tools/lib/subcmd" {
		t.Fatalf("split-root compound execution directory = %q, want source invocation directory", recipe.ExecutionDirectory)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if !strings.Contains(script, "'${tree:kernel}/"+source+"'") {
		t.Fatalf("split-root compound compiler lost immutable source provenance: %q", script)
	}
	if strings.Contains(script, " -c -o '"+target+"' "+source) || strings.Contains(script, " -c -o "+target+" "+source) {
		t.Fatalf("split-root compound compiler retained a staged source operand: %q", script)
	}
	if !slices.Contains(node.Trees, "kernel") || !slices.Contains(recipe.Trees, "kernel") {
		t.Fatalf("split-root compound compiler omits kernel tree binding: node=%#v recipe=%#v", node.Trees, recipe.Trees)
	}
	if got := recipe.WorkingOutputs["00000000"]; got != target {
		t.Fatalf("split-root compound output = %q, want writable object path %q", got, target)
	}
}

func TestGenericKbuildCrossDirectoryCompileProjectsDepfileParent(t *testing.T) {
	const (
		target  = "tools/objtool/librbtree.o"
		source  = "tools/lib/rbtree.c"
		depfile = "tools/objtool/.librbtree.o.d"
	)
	profile := mustCompactKbuildProfileForTest(t, "host:objtool", "tools/build/Makefile.build", "tools/objtool", `
cmd_cc_o_c = $(CC) -Wp,-MMD,$(obj)/.librbtree.o.d,-MT,$@ -c -o $@ $<
tools/objtool/librbtree.o: tools/lib/rbtree.c FORCE
	mkdir -p $(dir $@)
	$(call if_changed_dep,cc_o_c)
`, map[string]string{"CC": KbuildActionRoleToken("host", "cc")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("host", "host", "sdk").
		forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("objtool compile producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Kind != "compile" || node.Tool != "cc" || node.Stage != "host" {
		t.Fatalf("objtool compile node = %#v", node)
	}
	if got := recipe.WorkingOutputs["00000000"]; got != target {
		t.Fatalf("objtool compile working output = %q, want %q", got, target)
	}
	if !slices.Contains(recipe.Arguments, "-Wp,-MMD,${work:root}/"+depfile+",-MT,"+target) {
		t.Fatalf("objtool compile arguments = %#v, want evaluated depfile %q", recipe.Arguments, depfile)
	}
	if got := recipe.WorkingInputs["source:object:00000000"]; got != source {
		t.Fatalf("objtool compile source projection = %q, want cross-directory %q", got, source)
	}
}

func TestGenericKbuildCompileExpandsRecursiveCommandLineSourceRoot(t *testing.T) {
	const (
		target = "drivers/example/example.o"
		source = "drivers/example/example.c"
	)
	root := t.TempDir()
	mustWriteSource(t, root, source, "int example;\n")
	kb, err := parseKbuildWithOptions(strings.NewReader(`
ccflags-y := -I$(SDK)/linux/include
cmd_cc_o_c = $(CC) $(ccflags-y) -c -o $@ $<
drivers/example/example.o: drivers/example/example.c FORCE
	$(call if_changed,cc_o_c)
`), "Kbuild", KbuildOptions{
		Variables: map[string]string{"srctree": filepath.ToSlash(root)},
		CommandLineVariables: map[string]string{
			"CC":  KbuildActionRoleToken("target", "cc"),
			"SDK": "$(srctree)/vendor",
		},
		SourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
			"__LINUX_BZL_OBJECT_TREE__": root,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("build:example", "Kbuild", "", kb)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("compile node count=%d, want %d: %#v", got, want, plan.Nodes)
	}
	node := plan.Nodes[0]
	recipe := plan.Recipes[node.Recipe]
	if node.Kind != "compile" || node.Tool != "cc" || recipe.Tool != "cc" {
		t.Fatalf("recursive command-line include did not lower to a direct compile: node=%#v recipe=%#v", node, recipe)
	}
	if want := "-I${tree:kernel}/vendor/linux/include"; !slices.Contains(recipe.Arguments, want) {
		t.Fatalf("compile arguments=%#v, want projected recursive command-line include %q", recipe.Arguments, want)
	}
	if got, want := recipe.Environment["SDK"], "${tree:kernel}/vendor"; got != want {
		t.Fatalf("compile SDK environment=%q, want fully expanded and projected %q", got, want)
	}
	values := append(slices.Clone(recipe.Arguments), sortedStringMapValues(recipe.Environment)...)
	for _, value := range values {
		if strings.Contains(value, "$(") {
			t.Fatalf("compile recipe leaks residual Make reference into execution: %q", value)
		}
	}
}

func TestGenericKbuildCompilePreservesLogicalModuleMembershipAfterPathProjection(t *testing.T) {
	const (
		directory    = "drivers/example"
		moduleTarget = directory + "/ngbde_main.o"
		kernelTarget = directory + "/ordinary.o"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
KBUILD_CFLAGS_MODULE := -DMODULE
KBUILD_CFLAGS_KERNEL := -DKERNEL
real-obj-m := $(addprefix $(obj)/,ngbde_main.o)
part-of-module = $(if $(filter $(basename $@).o, $(real-obj-m)),y)
modkern_cflags = $(if $(part-of-module),$(KBUILD_CFLAGS_MODULE),$(KBUILD_CFLAGS_KERNEL))
c_flags = $(modkern_cflags)
cmd_cc_o_c = $(CC) $(c_flags) -c -o $@ $<
$(obj)/%.o: $(src)/%.c FORCE
	$(call if_changed,cc_o_c)
`, map[string]string{
		"CC":  KbuildActionRoleToken("target", "cc"),
		"obj": directory,
		"src": directory,
	})
	profile = compactKbuildProfileWithSourcesForTest(
		t, profile,
		strings.TrimSuffix(moduleTarget, ".o")+".c",
		strings.TrimSuffix(kernelTarget, ".o")+".c",
	)
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	for _, test := range []struct {
		name, target, selected, rejected string
	}{
		{name: "module member", target: moduleTarget, selected: "-DMODULE", rejected: "-DKERNEL"},
		{name: "non-member", target: kernelTarget, selected: "-DKERNEL", rejected: "-DMODULE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
			if err := buildCompactKbuildTargetForTest(metadata, plan, test.target); err != nil {
				t.Fatal(err)
			}
			if got, want := len(plan.Nodes), 1; got != want {
				t.Fatalf("compile node count=%d, want %d: %#v", got, want, plan.Nodes)
			}
			recipe := plan.Recipes[plan.Nodes[0].Recipe]
			if !slices.Contains(recipe.Arguments, test.selected) {
				t.Fatalf("compile arguments=%#v, want source-derived flag %q", recipe.Arguments, test.selected)
			}
			if slices.Contains(recipe.Arguments, test.rejected) {
				t.Fatalf("compile arguments=%#v unexpectedly selected %q", recipe.Arguments, test.rejected)
			}
		})
	}
}

func TestGenericKbuildActionUsesSourceExportedEnvironment(t *testing.T) {
	const (
		target = "drivers/example/selected.o"
		source = "drivers/example/selected.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:drivers/example", "scripts/Makefile.build", "drivers/example", `
export RUSTC_BOOTSTRAP := source-selected
export ROLE_FREE_FLAGS := --from-source
PRIVATE_VALUE := must-not-leak
export UNUSED_LINKER := $(LD)
cmd_cc_o_c = INLINE_VALUE=inline $(CC) -c -o $@ $<
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"),
		"LD": KbuildActionRoleToken("target", "ld"),
	})
	metadata := &CompactMetadata{actionRoles: testTargetActionRoles("cc", "ld")}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "selected", command: "cc_o_c",
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	for name, want := range map[string]string{
		"INLINE_VALUE":    "inline",
		"ROLE_FREE_FLAGS": "--from-source",
		"RUSTC_BOOTSTRAP": "source-selected",
	} {
		if got := recipe.Environment[name]; got != want {
			t.Errorf("%s = %q, want %q (environment %#v)", name, got, want, recipe.Environment)
		}
	}
	for _, name := range []string{"PRIVATE_VALUE", "UNUSED_LINKER"} {
		if _, exists := recipe.Environment[name]; exists {
			t.Errorf("unprojected %s leaked into environment %#v", name, recipe.Environment)
		}
	}
	if slices.Contains(recipe.AuxiliaryTools, "ld") {
		t.Fatalf("unused exported linker leaked as auxiliary capability: %#v", recipe.AuxiliaryTools)
	}
}

func TestGenericKbuildRecipePreservesQuotedEmptyArgument(t *testing.T) {
	commands, err := parseCompactKbuildRecipe(
		`scripts/mkcompile_h "" "Clang 22.1.8" "ld.lld" > $@`,
		compactKbuildAutomaticContext{target: "include/generated/compile.h"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(commands), 1; got != want {
		t.Fatalf("command count = %d, want %d: %#v", got, want, commands)
	}
	if got, want := commands[0].arguments, []string{"", "Clang 22.1.8", "ld.lld"}; !slices.Equal(got, want) {
		t.Fatalf("arguments = %#v, want %#v", got, want)
	}
}

func TestGenericKbuildRecipeAllowsAttachedDynamicTreePaths(t *testing.T) {
	commands, err := parseCompactKbuildRecipe(
		`cc -iquote${tree:host_deps}/external/elfutils -I${tree:host_deps}/external/elfutils/libelf -c input.c`,
		compactKbuildAutomaticContext{target: "tools/objtool/input.o"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := commands[0].arguments[:2], []string{
		"-iquote${tree:host_deps}/external/elfutils",
		"-I${tree:host_deps}/external/elfutils/libelf",
	}; !slices.Equal(got, want) {
		t.Fatalf("dynamic tree arguments = %q, want %q", got, want)
	}
}

func TestGenericKbuildResolveBtfidsHostCompileUsesComputedObjectFlags(t *testing.T) {
	const (
		directory   = "resolve_btfids"
		target      = "tools/bpf/resolve_btfids/string.o"
		source      = "tools/lib/string.c"
		libelfFlags = "-iquote__LINUX_BZL_HOST_DEPS__/external/elfutils+ " +
			"-I__LINUX_BZL_HOST_DEPS__/external/elfutils+/libelf"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "tools/build/Makefile.build", directory, `
comma := ,
dot-target = $(dir $@).$(notdir $@)
depfile = $(subst $(comma),_,$(dot-target).d)
basetarget = $(basename $(notdir $@))
LIBBPF_INCLUDE := $(OUTPUT)libbpf/include
SUBCMD_INCLUDE := $(OUTPUT)libsubcmd/include
HOSTCFLAGS_resolve_btfids += -g \
          -I$(srctree)/tools/include \
          -I$(srctree)/tools/include/uapi \
          -I$(LIBBPF_INCLUDE) \
          -I$(SUBCMD_INCLUDE) \
          $(LIBELF_FLAGS)
export srctree OUTPUT HOSTCFLAGS_resolve_btfids HOSTCC
host_c_flags = -Wp,-MD,$(depfile) -Wp,-MT,$@ $(HOSTCFLAGS) -D"BUILD_STR(s)=\#s" $(HOSTCFLAGS_$(basetarget).o) $(HOSTCFLAGS_$(obj))
cmd_mkdir = mkdir -p $(dir $@)
rule_mkdir = $(cmd_mkdir)
cmd_host_cc_o_c = $(HOSTCC) $(host_c_flags) -c -o $@ $<
$(OUTPUT)%.o: ../../lib/%.c FORCE
	$(call rule_mkdir)
	$(call if_changed_dep,host_cc_o_c)
`, map[string]string{
		"HOSTCC":       KbuildActionRoleToken("host", "cc"),
		"LIBELF_FLAGS": libelfFlags,
		"OUTPUT":       "__LINUX_BZL_OBJECT_TREE__/tools/bpf/resolve_btfids/",
		"obj":          directory,
		"srctree":      "__LINUX_BZL_SOURCE_TREE__",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: "tools/bpf/resolve_btfids",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("host", "host", "sdk").
		forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("resolve_btfids compile producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	wantArguments := []string{
		"-Wp,-MD,${work:root}/tools/bpf/resolve_btfids/.string.o.d",
		"-Wp,-MT,tools/bpf/resolve_btfids/string.o",
		"-DBUILD_STR(s)=#s",
		"-g",
		"-I${tree:kernel}/tools/include",
		"-I${tree:kernel}/tools/include/uapi",
		"-I${tree:prep}/tools/bpf/resolve_btfids/libbpf/include",
		"-I${tree:prep}/tools/bpf/resolve_btfids/libsubcmd/include",
		"-iquote${tree:host_deps}/external/elfutils+",
		"-I${tree:host_deps}/external/elfutils+/libelf",
		"-c", "-o", "${output:00000000}", "${source:object:00000000}",
	}
	if !slices.Equal(recipe.Arguments, wantArguments) {
		t.Fatalf("resolve_btfids arguments = %#v, want %#v", recipe.Arguments, wantArguments)
	}
	if node.Kind != "compile" || node.Tool != "cc" || node.Stage != "host" {
		t.Fatalf("resolve_btfids node = %#v", node)
	}
	if got := recipe.WorkingInputs["source:object:00000000"]; got != source {
		t.Fatalf("resolve_btfids source = %q, want %q", got, source)
	}
	for _, tree := range []string{"kernel", "prep", "host_deps"} {
		if !slices.Contains(node.Trees, tree) || !slices.Contains(recipe.Trees, tree) {
			t.Errorf("resolve_btfids %s tree is not declared: node=%#v recipe=%#v", tree, node.Trees, recipe.Trees)
		}
	}
}

func TestGenericKbuildRecipePreservesLiteralDollarRegexAnchor(t *testing.T) {
	commands, err := parseCompactKbuildRecipe(
		`/selected/nm input.o | sed -n -r -e 's/^([0-9a-fA-F]+) [ABCDGRSTVW] (.+)$/pa_\2 = \2;/p' | sort | uniq > $@`,
		compactKbuildAutomaticContext{target: "generated/pasyms.h"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(commands), 4; got != want {
		t.Fatalf("command count = %d, want %d: %#v", got, want, commands)
	}
	if got, want := commands[1].arguments, []string{"-n", "-r", "-e", `s/^([0-9a-fA-F]+) [ABCDGRSTVW] (.+)$/pa_\2 = \2;/p`}; !slices.Equal(got, want) {
		t.Fatalf("sed arguments = %#v, want %#v", got, want)
	}
}

func TestGenericKbuildRecipeUsesHermeticMulticallForBareUtilities(t *testing.T) {
	metadata, target := compactGenericRecipeMetadataForTest(
		t, `sed -n '$p' $< | sort | uniq > $@`, ":", "generated/result.h",
	)
	metadata.actionRoles = append(metadata.actionRoles, testScopedActionRoles(compactKbuildScriptRuntimeRole)...)
	plan := compactGenericRecipePlanForTest()
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 2; got != want {
		t.Fatalf("node count = %d, want %d: %#v", got, want, plan.Nodes)
	}
	node := plan.Nodes[1]
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole ||
		!slices.Contains(recipe.AuxiliaryTools, compactKbuildScriptRuntimeRole) {
		t.Fatalf("atomic utility pipeline tool = (%q, %q), auxiliary=%#v", node.Tool, recipe.Tool, recipe.AuxiliaryTools)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if !strings.Contains(script, `sed -n '' ${tree:kernel}/input.txt | sort | uniq > `+target) {
		t.Fatalf("atomic utility pipeline script = %q", script)
	}
}

func TestGenericKbuildHostProgramUsesHostActionRole(t *testing.T) {
	const (
		target            = "usr/gen_init_cpio"
		source            = target + ".c"
		configSource      = "autoconf.h"
		configWorkingPath = "include/generated/autoconf.h"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:usr", "scripts/Makefile.build", "usr", `
	cmd_host-csingle = $(HOSTCC) $(KBUILD_HOSTCFLAGS) -o $@ $<
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"), "HOSTCC": KbuildActionRoleToken("host", "cc"), "KBUILD_HOSTCFLAGS": "-DHOST_SELECTED -include " + configWorkingPath,
	})
	metadata := &CompactMetadata{actionRoles: testScopedActionRoles("cc")}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	configSourceID, err := ensureActionPlanSource(plan, "config", configSource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "prep", Kind: "copy", Tool: "actionfile", Product: "sdk",
		Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: configSourceID}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: configWorkingPath}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${source:input:00000000}", "-out", "${output:00000000}"},
		Sources:   []string{"input:00000000"}, Outputs: []string{"00000000"},
	}); err != nil {
		t.Fatal(err)
	}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forOutput("host", "host", "sdk")
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "gen_init_cpio", command: "host-csingle",
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("host producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Stage != "host" || node.Tool != "cc" || node.Outputs[0] != (ActionPlanOutput{Tree: "host", Path: target}) {
		t.Fatalf("host node = %#v", node)
	}
	if !slices.Contains(recipe.Arguments, "-DHOST_SELECTED") || strings.Contains(strings.Join(recipe.Arguments, " "), KbuildActionRoleToken("target", "cc")) {
		t.Fatalf("host recipe = %#v, want HOSTCC role with host flags", recipe)
	}
	if slices.ContainsFunc(node.Inputs, func(edge ActionPlanNodeEdge) bool {
		producer, ok := compactKbuildPlanNode(plan, edge.ProducerID)
		return ok && producer.Stage == "prep"
	}) {
		t.Fatalf("host recipe has a backward prep input: %#v", node.Inputs)
	}
	configBinding := ""
	for index, edge := range node.Sources {
		if edge.SourceID == configSourceID {
			configBinding = fmt.Sprintf("source:%s:%08d", edge.Role, index)
			break
		}
	}
	if configBinding == "" || recipe.WorkingInputs[configBinding] != configWorkingPath {
		t.Fatalf("host config source binding=%q working_inputs=%#v sources=%#v", configBinding, recipe.WorkingInputs, node.Sources)
	}
	configPlaceholder := "${source:" + strings.TrimPrefix(configBinding, "source:") + "}"
	boundInclude := false
	for index := 0; index+1 < len(recipe.Arguments); index++ {
		if recipe.Arguments[index] == "-include" && recipe.Arguments[index+1] == configPlaceholder {
			boundInclude = true
			break
		}
	}
	if !boundInclude {
		t.Fatalf("host argv does not bind -include to config source %q: %#v", configPlaceholder, recipe.Arguments)
	}
	plan.Toolsets = map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity}
	if err := contentAddressActionPlanNodes(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("stage-visible host config graph is invalid: %v", err)
	}
}

func TestExternalKbuildCompilerUsesStagedSourceOverlayForSrcInclude(t *testing.T) {
	const (
		directory = "external/module"
		target    = directory + "/module.o"
		source    = directory + "/module.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "external", "scripts/Makefile.build", directory, `
ccflags-y = -I$(src)
cmd_cc_o_c = $(CC) $(ccflags-y) -c -o $@ $<
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"),
	})
	kernelRoot := t.TempDir()
	externalRoot := t.TempDir()
	mustWriteSource(t, externalRoot, "module.c", "int module_source;\n")
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":              kernelRoot,
		"__LINUX_BZL_SOURCE_TREE__/" + directory: externalRoot,
	}
	// Linux 6.12 runs `make -f scripts/Makefile.build obj=$M` without
	// changing directory. The process therefore remains at the object-tree
	// root even though the evaluated profile and its targets live below M.
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testTargetActionRoles("cc"),
		sourceNamespaces: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__/" + directory: "external",
		},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "module", command: "cc_o_c",
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("external compile producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if !slices.Contains(recipe.Arguments, "-I"+directory) {
		t.Fatalf("external compiler arguments do not use the replayable staged source overlay: %#v", recipe.Arguments)
	}
	joined := strings.Join(append(slices.Clone(recipe.Arguments), sortedStringMapValues(recipe.Environment)...), " ")
	for _, rejected := range []string{
		"-I${work:root}/" + directory,
		"-I${tree:kernel}/" + directory,
		"$(src)", "$(srcroot)", "$(abs_output)", "__LINUX_BZL_",
	} {
		if strings.Contains(joined, rejected) {
			t.Fatalf("external compiler action retains rejected alias %q: arguments=%#v environment=%#v", rejected, recipe.Arguments, recipe.Environment)
		}
	}
	if !slices.Contains(sortedStringMapValues(recipe.WorkingInputs), source) {
		t.Fatalf("external source is not staged at %q: %#v", source, recipe.WorkingInputs)
	}
	if got, want := recipe.WorkingTrees, []string{"external"}; !slices.Equal(got, want) {
		t.Fatalf("external compiler working trees = %#v, want %#v", got, want)
	}
	if !slices.Contains(node.Trees, "external") || !slices.Contains(recipe.Trees, "external") {
		t.Fatalf("external compiler tree closure = node %#v recipe %#v, want external", node.Trees, recipe.Trees)
	}
}

func TestExternalKbuildCompilerStagesPreconfiguredObjectIncludeRoot(t *testing.T) {
	const (
		directory = ".linux-bzl/external/demo"
		target    = directory + "/.module-common.o"
		source    = "scripts/module-common.c"
		header    = "arch/x86/include/generated/asm/rwonce.h"
		outside   = "include/generated/unrelated.h"
	)
	profile := mustCompactKbuildProfileForTest(t, "external-modfinal", "scripts/Makefile.build", directory, `
cmd_cc_o_c = $(CC) -I$(objtree)/arch/x86/include/generated -c -o $@ $<
`, map[string]string{
		"CC":      KbuildActionRoleToken("target", "cc"),
		"objtree": "__LINUX_BZL_OBJECT_TREE__",
	})
	kernelRoot := t.TempDir()
	objectRoot := t.TempDir()
	externalRoot := t.TempDir()
	mustWriteSource(t, kernelRoot, source, "#include <asm/rwonce.h>\n")
	mustWriteSource(t, objectRoot, header, "#define READ_ONCE(x) (x)\n")
	mustWriteSource(t, objectRoot, outside, "#define UNRELATED 1\n")
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":              kernelRoot,
		"__LINUX_BZL_OBJECT_TREE__":              objectRoot,
		"__LINUX_BZL_SOURCE_TREE__/" + directory: externalRoot,
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles:             testTargetActionRoles("cc"),
		preconfiguredObjectTree: true,
		exactSourceNamespaces: map[string]string{
			header:  "prep",
			outside: "prep",
		},
		sourceNamespaces: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__/" + directory: "external",
		},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: ".module-common", command: "cc_o_c",
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("external compiler producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if !slices.Contains(recipe.Arguments, "-I${work:root}/arch/x86/include/generated") {
		t.Fatalf("external compiler arguments omit prepared include root: %#v", recipe.Arguments)
	}
	if got := recipe.WorkingInputs["source:object:00000000"]; got != source {
		t.Fatalf("external compiler direct working source = %q, want %q", got, source)
	}
	preparedSource := ActionPlanSource{}
	for _, candidate := range plan.Sources {
		if candidate.Namespace == "prep" && candidate.Path == header {
			preparedSource = candidate
		}
	}
	if preparedSource.ID == "" {
		t.Fatalf("external compiler sources = %#v, want prep/%s", plan.Sources, header)
	}
	if got, want := len(actionPlanNodeInputSetEntriesForTest(t, plan, node)), 1; got != want {
		t.Fatalf("external compiler persistent inputs = %#v, want prepared header only", actionPlanNodeInputSetEntriesForTest(t, plan, node))
	}
	preparedInput, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, node, header)
	wantPreparedInput := ActionPlanInputSetEntry{
		Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: header},
		SourceID: preparedSource.ID,
	}
	if !found || preparedInput != wantPreparedInput {
		t.Fatalf("external compiler persistent prepared header = (%#v, %t), want %#v", preparedInput, found, wantPreparedInput)
	}
	if unrelatedInput, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, node, outside); found {
		t.Fatalf("external compiler staged unrelated prepared root %q as %#v", outside, unrelatedInput)
	}
}

func TestExternalRustCompoundStagesPreparedTargetSpec(t *testing.T) {
	const (
		directory  = ".linux-bzl/external/demo"
		target     = directory + "/demo.o"
		source     = directory + "/demo.rs"
		targetSpec = "scripts/target.json"
	)
	profile := mustCompactKbuildProfileForTest(t, "external-rust", "scripts/Makefile.build", directory, `
objtree := __LINUX_BZL_OBJECT_TREE__
obj := __LINUX_BZL_OBJECT_TREE__/.linux-bzl/external/demo
cmd_rustc_o_rs = { $(RUSTC) --target=$(objtree)/scripts/target.json --emit=obj=$@ $<; }
`+target+`: `+source+` FORCE
	$(call if_changed,rustc_o_rs)
FORCE:
`, map[string]string{
		"RUSTC": KbuildActionRoleToken("target", "rustc"),
	})
	kernelRoot := t.TempDir()
	objectRoot := t.TempDir()
	externalRoot := t.TempDir()
	mustWriteSource(t, objectRoot, targetSpec, "{}\n")
	mustWriteSource(t, externalRoot, "demo.rs", "#![no_std]\n")
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":              kernelRoot,
		"__LINUX_BZL_OBJECT_TREE__":              objectRoot,
		"__LINUX_BZL_SOURCE_TREE__/" + directory: externalRoot,
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles:             testTargetActionRoles(append(testConfiguredActionRoles, "rustc")...),
		preconfiguredObjectTree: true,
		exactSourceNamespaces: map[string]string{
			targetSpec: "prep",
		},
		sourceNamespaces: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__/" + directory: "external",
		},
		Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing external Rust producer %q", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("external Rust producer = %#v recipe = %#v, want atomic scriptrun", node, recipe)
	}
	if recipe.ExecutionDirectory != directory {
		t.Fatalf("external Rust execution directory = %q, want %q", recipe.ExecutionDirectory, directory)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if want := "--target=../../../" + targetSpec; !strings.Contains(script, want) {
		t.Fatalf("external Rust script omits cwd-relative prepared target spec %q: %q", want, script)
	}

	preparedSource := false
	for index, edge := range node.Sources {
		var sourceDescriptor ActionPlanSource
		for _, candidate := range plan.Sources {
			if candidate.ID == edge.SourceID {
				sourceDescriptor = candidate
				break
			}
		}
		if sourceDescriptor.Namespace != "prep" || sourceDescriptor.Path != targetSpec {
			continue
		}
		// The statically discovered --target operand is a direct hermetic
		// prerequisite, not merely a file inherited from writable-tree closure.
		if edge.Role != "prerequisite" || index >= len(recipe.Sources) ||
			recipe.WorkingInputs["source:"+recipe.Sources[index]] != targetSpec {
			t.Fatalf("prepared target-spec edge = %#v recipe = %#v", edge, recipe)
		}
		preparedSource = true
	}
	if !preparedSource {
		t.Fatalf("external Rust node sources = %#v from %#v, want exact prep/%s input", node.Sources, plan.Sources, targetSpec)
	}
	if got, want := recipe.WorkingTrees, []string{"external"}; !slices.Equal(got, want) {
		t.Fatalf("external compound Rust working trees = %#v, want %#v", got, want)
	}
	if !slices.Contains(node.Trees, "external") || !slices.Contains(recipe.Trees, "external") {
		t.Fatalf("external compound Rust tree closure = node %#v recipe %#v, want external", node.Trees, recipe.Trees)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("external Rust target-spec plan is invalid: %v", err)
	}
}

func TestExternalRustLinearStagesPreparedTargetSpec(t *testing.T) {
	const (
		directory          = ".linux-bzl/external/linear"
		target             = directory + "/linear.o"
		source             = directory + "/linear.rs"
		targetSpec         = "scripts/custom-target.json"
		libraryDir         = "rust"
		selectedSDKLibrary = libraryDir + "/libselected_sdk.rmeta"
		nestedSDKLibrary   = libraryDir + "/nested/libignored.rmeta"
		siblingSDKLibrary  = "rust-other/libignored.rmeta"
		missingSDKLibrary  = libraryDir + "/libmissing.rmeta"
	)
	profile := mustCompactKbuildProfileForTest(t, "external-rust-linear", "scripts/Makefile.build", directory, `
objtree := __LINUX_BZL_OBJECT_TREE__
cmd_rustc_o_rs = $(RUSTC) --target $(objtree)/scripts/custom-target.json -L $(objtree)/rust --emit=obj=$@ $<
`+target+`: `+source+` FORCE
	$(call if_changed,rustc_o_rs)
FORCE:
`, map[string]string{
		"RUSTC": KbuildActionRoleToken("target", "rustc"),
	})
	objectRoot := t.TempDir()
	externalRoot := t.TempDir()
	mustWriteSource(t, objectRoot, targetSpec, "{}\n")
	mustWriteSource(t, objectRoot, selectedSDKLibrary, "selected SDK metadata\n")
	mustWriteSource(t, objectRoot, nestedSDKLibrary, "nested SDK metadata\n")
	mustWriteSource(t, objectRoot, siblingSDKLibrary, "sibling SDK metadata\n")
	mustWriteSource(t, externalRoot, "linear.rs", "#![no_std]\n")
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":              t.TempDir(),
		"__LINUX_BZL_OBJECT_TREE__":              objectRoot,
		"__LINUX_BZL_SOURCE_TREE__/" + directory: externalRoot,
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles:             testTargetActionRoles(append(testConfiguredActionRoles, "rustc")...),
		preconfiguredObjectTree: true,
		exactSourceNamespaces: map[string]string{
			targetSpec:         "prep",
			selectedSDKLibrary: "prep",
			nestedSDKLibrary:   "prep",
			siblingSDKLibrary:  "prep",
			missingSDKLibrary:  "prep",
		},
		sourceNamespaces: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__/" + directory: "external",
		},
		Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing external linear Rust producer %q", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != "rustc" || recipe.Tool != "rustc" {
		t.Fatalf("external linear Rust producer = %#v recipe = %#v, want rustc", node, recipe)
	}
	directSource := false
	for index, edge := range node.Sources {
		for _, sourceDescriptor := range plan.Sources {
			if sourceDescriptor.ID != edge.SourceID || sourceDescriptor.Namespace != "external" || sourceDescriptor.Path != source {
				continue
			}
			if edge.Role != "object" || index >= len(recipe.Sources) {
				t.Fatalf("external linear Rust direct source edge = %#v recipe = %#v", edge, recipe)
			}
			binding := recipe.Sources[index]
			if recipe.WorkingInputs["source:"+binding] != source {
				t.Fatalf("external linear Rust direct source binding %q = %q, want %q", binding, recipe.WorkingInputs["source:"+binding], source)
			}
			directSource = true
		}
	}
	if !directSource {
		t.Fatalf("external linear Rust sources = %#v from %#v, want direct external/%s input", node.Sources, plan.Sources, source)
	}
	preparedSources := map[string]string{}
	for _, sourceDescriptor := range plan.Sources {
		if sourceDescriptor.Namespace == "prep" {
			preparedSources[sourceDescriptor.Path] = sourceDescriptor.ID
		}
	}
	if got, want := len(actionPlanNodeInputSetEntriesForTest(t, plan, node)), 2; got != want {
		t.Fatalf("external linear Rust persistent inputs = %#v, want target spec and selected SDK library", actionPlanNodeInputSetEntriesForTest(t, plan, node))
	}
	for _, pathname := range []string{targetSpec, selectedSDKLibrary} {
		got, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, node, pathname)
		want := ActionPlanInputSetEntry{
			Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: pathname},
			SourceID: preparedSources[pathname],
		}
		if want.SourceID == "" || !found || got != want {
			t.Fatalf("external linear Rust persistent input for %q = (%#v, %t), want %#v from sources %#v", pathname, got, found, want, plan.Sources)
		}
	}
	targetArgumentBound := false
	for argument := 0; argument+1 < len(recipe.Arguments); argument++ {
		if recipe.Arguments[argument] == "--target" && recipe.Arguments[argument+1] == "${work:root}/"+targetSpec {
			targetArgumentBound = true
		}
	}
	if !targetArgumentBound {
		t.Fatalf("external linear Rust arguments = %#v, want --target bound to persistent prep/%s work path", recipe.Arguments, targetSpec)
	}
	unexpectedLibraries := map[string]bool{
		nestedSDKLibrary:  true,
		siblingSDKLibrary: true,
		missingSDKLibrary: true,
	}
	for pathname := range unexpectedLibraries {
		if entry, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, node, pathname); found {
			t.Fatalf("external linear Rust unexpectedly stages %q from -L %s as %#v", pathname, libraryDir, entry)
		}
	}
	if got, want := recipe.WorkingTrees, []string{"external"}; !slices.Equal(got, want) {
		t.Fatalf("external linear Rust working trees = %#v, want %#v", got, want)
	}
	if !slices.Contains(node.Trees, "external") || !slices.Contains(recipe.Trees, "external") {
		t.Fatalf("external linear Rust tree closure = node %#v recipe %#v, want external", node.Trees, recipe.Trees)
	}
	if err := plan.exportReachableActionPlanInputSets(); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("external linear Rust -L closure plan is invalid: %v", err)
	}
}

func TestCompactKbuildCanonicalRootsPreserveNestedSourceOverlayProvenance(t *testing.T) {
	const directory = "vendor/demo"
	profile := mustCompactKbuildProfileForTest(t, "canonical-overlay", "scripts/Makefile.build", directory, `
cmd_copy = cp $< $@
`, nil)
	kernelPhysicalRoot := filepath.ToSlash(t.TempDir())
	kernelRoot := filepath.Join(t.TempDir(), "kernel")
	if err := os.Symlink(filepath.FromSlash(kernelPhysicalRoot), kernelRoot); err != nil {
		t.Fatal(err)
	}
	kernelRoot = filepath.ToSlash(kernelRoot)
	objectRoot := filepath.ToSlash(t.TempDir())
	hostDepsRoot := filepath.ToSlash(t.TempDir())
	overlayRoot := filepath.ToSlash(t.TempDir())
	virtualOverlay := "__LINUX_BZL_SOURCE_TREE__/" + directory
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__": kernelRoot,
		"__LINUX_BZL_OBJECT_TREE__": objectRoot,
		linuxProbeHostDepsSentinel:  hostDepsRoot,
		virtualOverlay:              overlayRoot,
	}

	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "virtual root", input: virtualOverlay, want: "${tree:prep}/" + directory},
		{name: "virtual child", input: virtualOverlay + "/src/module.c", want: "${tree:prep}/" + directory + "/src/module.c"},
		{name: "virtual M assignment", input: "M=" + virtualOverlay, want: "M=${tree:prep}/" + directory},
		{name: "physical root", input: overlayRoot, want: "${tree:prep}/" + directory},
		{name: "physical KBUILD_EXTMOD assignment", input: "KBUILD_EXTMOD=" + overlayRoot + "/src", want: "KBUILD_EXTMOD=${tree:prep}/" + directory + "/src"},
		{name: "physical output assignment", input: "output=" + overlayRoot + "/generated", want: "output=${tree:prep}/" + directory + "/generated"},
		{name: "kernel root", input: kernelRoot + "/scripts/mod/modpost.c", want: "${tree:kernel}/scripts/mod/modpost.c"},
		{name: "resolved kernel root", input: kernelPhysicalRoot + "/scripts/mod/modpost.c", want: "${tree:kernel}/scripts/mod/modpost.c"},
		{name: "object root", input: objectRoot + "/include/generated/autoconf.h", want: "${tree:prep}/include/generated/autoconf.h"},
		{name: "host dependency root", input: hostDepsRoot + "/bin/tool", want: "${tree:" + linuxProbeHostDepsRootName + "}/bin/tool"},
		{name: "component prefix collision", input: overlayRoot + "-other/src", want: overlayRoot + "-other/src"},
	} {
		t.Run("recipe/"+test.name, func(t *testing.T) {
			if got := compactKbuildProfileCanonicalRecipeText(profile, test.input); got != test.want {
				t.Fatalf("canonical recipe text = %q, want %q", got, test.want)
			}
		})
	}

	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "physical root", input: overlayRoot, want: virtualOverlay},
		{name: "physical child", input: overlayRoot + "/src/module.c", want: virtualOverlay + "/src/module.c"},
		{name: "physical M assignment", input: "M=" + overlayRoot, want: "M=" + virtualOverlay},
	} {
		t.Run("make/"+test.name, func(t *testing.T) {
			if got := compactKbuildProfileCanonicalMakeValue(profile, test.input); got != test.want {
				t.Fatalf("canonical Make value = %q, want %q", got, test.want)
			}
		})
	}

	privateOverlay := compactKbuildActionObjectTreeMarker + "/" + directory
	if got, want := compactKbuildProfileRootedActionRecipeText(profile, "M="+overlayRoot+"/src"), "M="+privateOverlay+"/src"; got != want {
		t.Fatalf("rooted overlay value = %q, want %q", got, want)
	}
	if got := compactKbuildProfileRootedActionRecipeText(profile, "M="+virtualOverlay); got != "M="+virtualOverlay {
		t.Fatalf("rooted public overlay marker = %q, want source payload unchanged", got)
	}
	if got, want := compactKbuildProfileEvaluatedRootedActionRecipeText(profile, "M="+virtualOverlay+"/src"), "M="+privateOverlay+"/src"; got != want {
		t.Fatalf("evaluated rooted overlay value = %q, want %q", got, want)
	} else if finalized := compactKbuildFinalizeRootedActionRecipeText(got); finalized != "M=${tree:prep}/"+directory+"/src" {
		t.Fatalf("finalized evaluated overlay value = %q", finalized)
	}
	if got, want := compactKbuildSourceScriptReplayValue(profile, "M="+overlayRoot), "M=${work:root}/"+directory; got != want {
		t.Fatalf("physical overlay replay root = %q, want %q", got, want)
	}

	for _, variable := range []string{"M", "KBUILD_EXTMOD", "output"} {
		input := variable + "=" + virtualOverlay + "/src"
		want := variable + "=${work:root}/" + directory + "/src"
		if got := compactKbuildSourceScriptReplayValue(profile, input); got != want {
			t.Errorf("replay %s = %q, want %q", variable, got, want)
		}
	}
}

func TestCompactKbuildCanonicalRootsAcceptExecrootRelativeTreeArtifacts(t *testing.T) {
	const directory = ".linux-bzl/external/demo"
	profile := mustCompactKbuildProfileForTest(t, "canonical-relative-overlay", "scripts/Makefile.build", directory, `
cmd_copy = cp $< $@
`, nil)
	const (
		kernelRoot  = "external/linux-source"
		objectRoot  = "bazel-out/cfg/bin/external/linux/kernel.tree-sdk"
		overlayRoot = "bazel-out/cfg/bin/demo.external-source/" + directory
		rustRoot    = "external/rust-src/library"
	)
	virtualOverlay := "__LINUX_BZL_SOURCE_TREE__/" + directory
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__": kernelRoot,
		"__LINUX_BZL_OBJECT_TREE__": objectRoot,
		virtualOverlay:              overlayRoot,
		rustRoot:                    rustRoot,
	}

	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "relative object recipe",
			input: "obj=" + objectRoot + "/" + directory,
			want:  "obj=${tree:prep}/" + directory,
		},
		{
			name:  "relative overlay recipe",
			input: "src=" + overlayRoot,
			want:  "src=${tree:prep}/" + directory,
		},
		{
			name:  "joined relative include option",
			input: "-I" + kernelRoot + "/include",
			want:  "-I${tree:kernel}/include",
		},
		{
			name:  "quoted exact relative root",
			input: `cd "` + kernelRoot + `"`,
			want:  `cd "${tree:kernel}"`,
		},
		{
			name:  "relative root before shell connector",
			input: kernelRoot + "; next",
			want:  "${tree:kernel}; next",
		},
		{
			name:  "relative root in path list",
			input: "PATH=" + kernelRoot + ":$PATH",
			want:  "PATH=${tree:kernel}:$PATH",
		},
		{
			name:  "relative root in comma list",
			input: "roots=" + kernelRoot + ",other",
			want:  "roots=${tree:kernel},other",
		},
		{
			name:  "embedded relative root is not provenance",
			input: "path=not" + kernelRoot + "/Makefile",
			want:  "path=not" + kernelRoot + "/Makefile",
		},
		{
			name:  "relative root suffix collision",
			input: "path=" + kernelRoot + "-other/Makefile",
			want:  "path=" + kernelRoot + "-other/Makefile",
		},
		{
			name:  "independent Rust source root",
			input: "crate=" + rustRoot + "/core/src/lib.rs",
			want:  "crate=" + rustRoot + "/core/src/lib.rs",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := compactKbuildProfileCanonicalRecipeText(profile, test.input); got != test.want {
				t.Fatalf("canonical recipe text = %q, want %q", got, test.want)
			}
		})
	}

	if got, want := compactKbuildProfileCanonicalMakeValue(profile, "M="+overlayRoot), "M="+virtualOverlay; got != want {
		t.Fatalf("canonical recursive Make value = %q, want %q", got, want)
	}
}

func TestParsedKbuildCanonicalizesNestedSourceOverlayMarker(t *testing.T) {
	const directory = "external/module"
	physical := filepath.ToSlash(t.TempDir())
	kb, err := parseKbuildWithOptions(strings.NewReader("all:\n\ttouch $@\n"), "Makefile", KbuildOptions{
		SourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__/" + directory + "/.": physical,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("canonical-overlay-key", "Makefile", "", kb)
	if err != nil {
		t.Fatal(err)
	}
	profile.Directory = directory
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	root, overlay, err := compactKbuildSourceOverlayRoot(profile)
	if err != nil || !overlay || root != directory {
		t.Fatalf("canonical source overlay root = (%q, %t, %v), want (%q, true, nil)", root, overlay, err, directory)
	}
	projections := compactKbuildProfileSourceOverlayProjections(profile)
	if len(projections) != 1 || projections[0].virtual != "__LINUX_BZL_SOURCE_TREE__/"+directory ||
		projections[0].prefix != directory || !slices.Contains(projections[0].physical, physical) {
		t.Fatalf("canonical source overlay projections = %#v", projections)
	}

	for _, sourceRoots := range []map[string]string{
		{
			"__LINUX_BZL_SOURCE_TREE__/" + directory:        physical,
			"__LINUX_BZL_SOURCE_TREE__/" + directory + "/.": physical,
		},
		{"__LINUX_BZL_SOURCE_TREE__/../escape": physical},
	} {
		if _, err := normalizeKbuildSourceRoots(sourceRoots); err == nil {
			t.Fatalf("invalid nested source roots accepted: %#v", sourceRoots)
		}
	}
}

func TestGenericKbuildHostLinkDriverStagesResolvedConfigBaseline(t *testing.T) {
	const (
		target     = "lib/crc/gen_crc32table"
		source     = target + ".c"
		configPath = "include/generated/autoconf.h"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:lib", "scripts/Makefile.build", "lib", `
cmd_host-csingle = $(HOSTCC) -I$(objtree)/lib/crc -o $@ $<
lib/crc/gen_crc32table: lib/crc/gen_crc32table.c FORCE
	$(call if_changed,host-csingle)
`, map[string]string{
		"HOSTCC":  KbuildActionRoleToken("host", "cc"),
		"objtree": "__LINUX_BZL_OBJECT_TREE__",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testHostActionRoles("cc"),
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	configSource, err := ensureActionPlanSource(plan, "config", "autoconf.h")
	if err != nil {
		t.Fatal(err)
	}
	_, err = appendActionPlanNode(plan, ActionPlanNode{
		Stage: "prep", Kind: "copy", Tool: "actionfile", Product: "sdk",
		Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: configSource}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: configPath}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
		Arguments: []string{"-input", "${source:input:00000000}", "-out", "${output:00000000}"},
		Sources:   []string{"input:00000000"}, Outputs: []string{"00000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("host", "host", "sdk").
		withInitialObjectTree(true).
		forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("host link-driver producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Kind != "link-driver" || node.Tool != "cc" || node.Stage != "host" {
		t.Fatalf("host link-driver node = %#v", node)
	}
	if !slices.Contains(recipe.Arguments, "-I${work:root}/lib/crc") ||
		strings.Contains(strings.Join(recipe.Arguments, " "), "${tree:prep}") {
		t.Fatalf("host link-driver arguments do not use writable include overlay: %#v", recipe.Arguments)
	}
	directSource := ActionPlanSource{}
	for _, candidate := range plan.Sources {
		if candidate.Path == source {
			directSource = candidate
			break
		}
	}
	if directSource.ID == "" || !slices.ContainsFunc(node.Sources, func(input ActionPlanSourceEdge) bool {
		return input.Role == "object" && input.SourceID == directSource.ID
	}) {
		t.Fatalf("host link-driver sources omit direct %q prerequisite: node=%#v sources=%#v", source, node.Sources, plan.Sources)
	}
	if got, want := len(actionPlanNodeInputSetEntriesForTest(t, plan, node)), 1; got != want {
		t.Fatalf("host link-driver persistent inputs = %#v, want resolved config baseline only", actionPlanNodeInputSetEntriesForTest(t, plan, node))
	}
	configInput, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, node, configPath)
	wantConfigInput := ActionPlanInputSetEntry{
		Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: configPath},
		SourceID: configSource,
	}
	if !found || configInput != wantConfigInput {
		t.Fatalf("host link-driver resolved config input = (%#v, %t), want %#v", configInput, found, wantConfigInput)
	}
}

func TestGenericKbuildVDSOHostRecipeRetainsSourceAndDeclaresKernelTree(t *testing.T) {
	const (
		directory = "arch/x86/entry/vdso"
		target    = directory + "/vdso2c"
		source    = target + ".c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
	KBUILD_HOSTCFLAGS = -I$(srctree)/scripts/include
	cmd_host-csingle = $(HOSTCC) $(KBUILD_HOSTCFLAGS) -o $@ $<
`, map[string]string{
		"HOSTCC": KbuildActionRoleToken("host", "cc"),
	})
	metadata := &CompactMetadata{actionRoles: testScopedActionRoles("cc")}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forOutput("host", "host", "sdk")
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "vdso2c", command: "host-csingle",
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("vdso2c producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]

	sourceBinding := ""
	for index, edge := range node.Sources {
		if edge.SourceID == sourceID {
			sourceBinding = fmt.Sprintf("%s:%08d", edge.Role, index)
			break
		}
	}
	if sourceBinding == "" {
		t.Fatalf("vdso2c source %q is not bound: %#v", sourceID, node.Sources)
	}
	if got, want := recipe.WorkingInputs["source:"+sourceBinding], source; got != want {
		t.Fatalf("vdso2c working source = %q, want immutable source path %q", got, want)
	}
	if placeholder := "${source:" + sourceBinding + "}"; !slices.Contains(recipe.Arguments, placeholder) {
		t.Fatalf("vdso2c argv does not retain immutable source %q: %#v", placeholder, recipe.Arguments)
	}
	if !slices.Contains(recipe.Arguments, "-I${tree:kernel}/scripts/include") {
		t.Fatalf("vdso2c argv does not retain source-tree include: %#v", recipe.Arguments)
	}
	if !slices.Contains(node.Trees, "kernel") || !slices.Contains(recipe.Trees, "kernel") {
		t.Fatalf("vdso2c source-tree closure is not declared: node=%#v recipe=%#v", node.Trees, recipe.Trees)
	}
}

func TestGenericKbuildPretargetCompileStagesExactVisibleFrontier(t *testing.T) {
	const (
		target            = "scripts/mod/devicetable-offsets.s"
		source            = "scripts/mod/devicetable-offsets.c"
		visibleHeader     = "arch/x86/include/generated/uapi/asm/types.h"
		configInput       = "autoconf.h"
		configWorkingPath = "include/generated/autoconf.h"
		unrelated         = "include/generated/unrelated.h"
	)
	profile := mustCompactKbuildProfileForTest(t, "scripts/mod", "scripts/Makefile.build", "scripts/mod", `
c_flags = $(KBUILD_CPPFLAGS) $(CC_FLAGS_LTO)
cmd_cc_s_c = $(CC) $(filter-out $(DEBUG_CFLAGS) $(CC_FLAGS_LTO), $(c_flags)) -fverbose-asm -S -o $@ $<
`, map[string]string{
		"CC":              KbuildActionRoleToken("target", "cc"),
		"KBUILD_CPPFLAGS": "-I__LINUX_BZL_SOURCE_TREE__/include -I__LINUX_BZL_OBJECT_TREE__/include -I__LINUX_BZL_OBJECT_TREE__/arch/x86/include/generated/uapi",
		"CC_FLAGS_LTO":    "-flto",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	visibleArtifact := CompactKbuildVisibleArtifact{
		Path: visibleHeader, Profile: "arch-headers", Target: visibleHeader,
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []CompactKbuildVisibleArtifact{visibleArtifact})

	headerProducer := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "bootstrap", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "bootstrap", Path: visibleHeader}},
	}
	unrelatedPrep := ActionPlanNode{
		ID: strings.Repeat("d", 64), Stage: "prep", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: unrelated}},
	}
	metadata := &CompactMetadata{actionRoles: testScopedActionRoles("cc")}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	configSourceID, err := ensureActionPlanSource(plan, "config", configInput)
	if err != nil {
		t.Fatal(err)
	}
	configProjection := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "prep", Kind: "copy", Tool: "actionfile", Product: "sdk",
		Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: configSourceID}},
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: configWorkingPath}},
	}
	plan.Nodes = []ActionPlanNode{headerProducer, configProjection, unrelatedPrep}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("bootstrap", "bootstrap", "sdk")
	builder = withMaterializedInitialObjectTreeArtifactForTest(
		t, builder, visibleArtifact, "bootstrap", "target", "target", headerProducer.ID,
	).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "devicetable-offsets", command: "cc_s_c",
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("bootstrap compile producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Stage != "bootstrap" || node.Tool != "cc" {
		t.Fatalf("bootstrap compile node=%#v", node)
	}
	if !slices.Contains(recipe.Arguments, "-I${work:root}/arch/x86/include/generated/uapi") {
		t.Fatalf("bootstrap compile arguments=%#v, want private object-tree include", recipe.Arguments)
	}
	sourceInclude := slices.Index(recipe.Arguments, "-I${tree:kernel}/include")
	objectInclude := slices.Index(recipe.Arguments, "-I${work:root}/include")
	if sourceInclude < 0 || objectInclude < 0 || sourceInclude >= objectInclude {
		t.Fatalf("bootstrap compile arguments=%#v, want ordered source then object include roots", recipe.Arguments)
	}
	if !slices.Contains(node.Trees, "kernel") || !slices.Contains(recipe.Trees, "kernel") {
		t.Fatalf("bootstrap compile omits kernel source tree: node=%#v recipe=%#v", node.Trees, recipe.Trees)
	}
	if strings.Contains(strings.Join(append(recipe.Arguments, sortedStringMapValues(recipe.Environment)...), " "), "${tree:prep}") {
		t.Fatalf("bootstrap compile retained immutable prep marker: %#v", recipe)
	}
	if got, want := len(actionPlanNodeInputSetEntriesForTest(t, plan, node)), 2; got != want {
		t.Fatalf("bootstrap persistent inputs=%#v, want visible header and config baseline", actionPlanNodeInputSetEntriesForTest(t, plan, node))
	}
	for pathname, want := range map[string]ActionPlanInputSetEntry{
		visibleHeader: {
			Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: visibleHeader},
			ProducerID: headerProducer.ID,
		},
		configWorkingPath: {
			Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: configWorkingPath},
			SourceID: configSourceID,
		},
	} {
		got, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, node, pathname)
		if !found || got != want {
			t.Fatalf("bootstrap persistent input for %q = (%#v, %t), want %#v", pathname, got, found, want)
		}
	}
	if unrelatedInput, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, node, unrelated); found {
		t.Fatalf("bootstrap persistent inputs include unrelated prep output %#v", unrelatedInput)
	}
	if !slices.ContainsFunc(node.Sources, func(edge ActionPlanSourceEdge) bool {
		return edge.Role == "object" && edge.SourceID == sourceID
	}) {
		t.Fatalf("bootstrap compile sources=%#v, want direct source prerequisite %s", node.Sources, sourceID)
	}
}

func TestKbuildWorkingTreeClosureRebasesSelectedUnmaterializedConfigProjection(t *testing.T) {
	const (
		configInput  = "kernel.release"
		configOutput = "include/config/kernel.release"
		consumer     = "scripts/mod/empty.o"
	)
	prepProfile := CompactKbuildProfile{
		Name: "prep-config", Path: "Makefile", EntryTargets: []string{configOutput},
	}
	for _, test := range []struct {
		stage     string
		lifecycle string
		scope     string
	}{
		{stage: "prehost", lifecycle: "target", scope: "host"},
		{stage: "bootstrap", lifecycle: "target", scope: "target"},
		{stage: "host", lifecycle: "target", scope: "host"},
		{stage: "prep", lifecycle: "prep", scope: "target"},
		{stage: "target", lifecycle: "target", scope: "target"},
	} {
		t.Run(test.stage, func(t *testing.T) {
			stage := test.stage
			consumerProfile := CompactKbuildProfile{
				Name: stage + "-consumer", Path: "scripts/Makefile.build", EntryTargets: []string{consumer},
			}
			config := CompactConfig{
				KbuildProfiles: []CompactKbuildProfile{prepProfile, consumerProfile},
				KbuildSelections: []CompactKbuildSelection{
					{Profile: prepProfile.Name, Target: configOutput, MakeTarget: configOutput, Lifecycle: "prep", Scope: "target", Stage: "prep"},
					{Profile: consumerProfile.Name, Target: consumer, MakeTarget: consumer, Lifecycle: test.lifecycle, Scope: test.scope, Stage: stage},
				},
			}
			graph, err := newCompactKbuildSelectionGraph(config)
			if err != nil {
				t.Fatal(err)
			}
			plan := &ActionPlan{}
			configSourceID, err := ensureActionPlanSource(plan, "config", configInput)
			if err != nil {
				t.Fatal(err)
			}
			plan.Nodes = []ActionPlanNode{{
				ID: strings.Repeat("a", 64), Stage: "prep", Kind: "copy", Tool: "actionfile", Product: "sdk",
				Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: configSourceID}},
				Outputs: []ActionPlanOutput{{Tree: "prep", Path: configOutput}},
			}}
			consumerKey := compactKbuildSelectionKey{
				profile: consumerProfile.Name, target: consumer, stage: stage,
			}
			builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
				withSelectionGraph(graph).
				forSelection(consumerKey, consumerProfile).
				forOutput(stage, stage, "sdk")
			inputs, err := builder.compactKbuildWorkingTreeClosureInputs(consumer, consumerProfile, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(inputs) != 1 {
				t.Fatalf("%s working-tree inputs = %#v, want one resolved config projection", stage, inputs)
			}
			input := inputs[0]
			if input.path != configOutput || input.sourceID != configSourceID || !input.objectTree || !input.workingOnly || input.producer != "" {
				t.Fatalf("%s resolved config input = %#v, want immutable config source %q at %q", stage, input, configSourceID, configOutput)
			}
			prepOwner := compactKbuildSelectionKey{
				profile: prepProfile.Name, target: configOutput, stage: "prep",
			}
			if _, materialized := graph.materializedProducers[prepOwner]; materialized {
				t.Fatalf("prep owner %s unexpectedly materialized", compactKbuildSelectionKeyString(prepOwner))
			}
		})
	}
}

func TestKbuildWorkingTreeClosurePrefersMaterializedConfigProjectionForPrepAndTarget(t *testing.T) {
	const (
		configInput  = "kernel.release"
		configOutput = "include/config/kernel.release"
		consumer     = "include/generated/timeconst.h"
	)
	prepProfile := CompactKbuildProfile{
		Name: "prep-config", Path: "Makefile", EntryTargets: []string{configOutput},
	}
	for _, test := range []struct {
		stage     string
		lifecycle string
		scope     string
	}{
		{stage: "prep", lifecycle: "prep", scope: "target"},
		{stage: "target", lifecycle: "target", scope: "target"},
	} {
		t.Run(test.stage, func(t *testing.T) {
			stage := test.stage
			consumerProfile := CompactKbuildProfile{
				Name: stage + "-consumer", Path: "Makefile", EntryTargets: []string{consumer},
			}
			config := CompactConfig{
				KbuildProfiles: []CompactKbuildProfile{prepProfile, consumerProfile},
				KbuildSelections: []CompactKbuildSelection{
					{Profile: prepProfile.Name, Target: configOutput, MakeTarget: configOutput, Lifecycle: "prep", Scope: "target", Stage: "prep"},
					{Profile: consumerProfile.Name, Target: consumer, MakeTarget: consumer, Lifecycle: test.lifecycle, Scope: test.scope, Stage: stage},
				},
			}
			graph, err := newCompactKbuildSelectionGraph(config)
			if err != nil {
				t.Fatal(err)
			}
			plan := &ActionPlan{}
			configSourceID, err := ensureActionPlanSource(plan, "config", configInput)
			if err != nil {
				t.Fatal(err)
			}
			projection := ActionPlanNode{
				ID: strings.Repeat("a", 64), Stage: "prep", Kind: "copy", Tool: "actionfile", Product: "sdk",
				Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: configSourceID}},
				Outputs: []ActionPlanOutput{{Tree: "prep", Path: configOutput}},
			}
			plan.Nodes = []ActionPlanNode{projection}
			prepOwner := compactKbuildSelectionKey{
				profile: prepProfile.Name, target: configOutput, stage: "prep",
			}
			if err := graph.recordMaterializedProducer(prepOwner, projection.ID); err != nil {
				t.Fatal(err)
			}
			consumerKey := compactKbuildSelectionKey{
				profile: consumerProfile.Name, target: consumer, stage: stage,
			}
			builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
				withSelectionGraph(graph).
				forSelection(consumerKey, consumerProfile).
				forOutput(stage, stage, "sdk")
			inputs, err := builder.compactKbuildWorkingTreeClosureInputs(consumer, consumerProfile, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(inputs) != 1 {
				t.Fatalf("%s working-tree inputs = %#v, want one resolved config projection", stage, inputs)
			}
			input := inputs[0]
			if input.path != configOutput || input.producer != projection.ID || input.slot != 0 || !input.workingOnly || input.sourceID != "" {
				t.Fatalf("%s resolved config input = %#v, want materialized producer %q", stage, input, projection.ID)
			}
		})
	}
}

func TestKbuildWorkingTreeClosureUsesConfigSourceWhenNoWriterIsSelected(t *testing.T) {
	for _, projection := range resolvedConfigProjections() {
		projection := projection
		for _, test := range []struct {
			stage     string
			lifecycle string
			scope     string
		}{
			{stage: "prep", lifecycle: "prep", scope: "target"},
			{stage: "target", lifecycle: "target", scope: "target"},
		} {
			t.Run(test.stage+"/"+strings.ReplaceAll(projection.output, "/", "_"), func(t *testing.T) {
				stage := test.stage
				consumer := "consumer-" + stage
				profile := CompactKbuildProfile{
					Name: stage + "-consumer", Path: "Makefile", EntryTargets: []string{consumer},
				}
				config := CompactConfig{
					KbuildProfiles: []CompactKbuildProfile{profile},
					KbuildSelections: []CompactKbuildSelection{{
						Profile: profile.Name, Target: consumer, MakeTarget: consumer, Lifecycle: test.lifecycle, Scope: test.scope, Stage: stage,
					}},
				}
				graph, err := newCompactKbuildSelectionGraph(config)
				if err != nil {
					t.Fatal(err)
				}
				plan := &ActionPlan{}
				sourceID, err := ensureActionPlanSource(plan, "config", projection.input)
				if err != nil {
					t.Fatal(err)
				}
				builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
					withSelectionGraph(graph).
					forSelection(compactKbuildSelectionKey{
						profile: profile.Name, target: consumer, stage: stage,
					}, profile).
					forOutput(stage, stage, "sdk")
				inputs, err := builder.compactKbuildWorkingTreeClosureInputs(consumer, profile, nil)
				if err != nil {
					t.Fatal(err)
				}
				if len(inputs) != 1 {
					t.Fatalf("working-tree inputs = %#v, want one source-backed config projection", inputs)
				}
				input := inputs[0]
				if input.path != projection.output || input.sourceID != sourceID || input.producer != "" || !input.objectTree || !input.workingOnly {
					t.Fatalf("source-backed config projection = %#v, want config/%s at %s", input, projection.input, projection.output)
				}
			})
		}
	}
}

func TestGenericKbuildRuleEvaluatesNestedIfChangedCommand(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:usr", "scripts/Makefile.build", "usr", `
compress-y := copy
cmd_copy = $(OBJCOPY) -o $@ $<
$(obj)/initramfs_inc_data: $(obj)/initramfs_data.cpio FORCE
	$(call if_changed,$(compress-y))
`, map[string]string{"obj": "usr", "OBJCOPY": KbuildActionRoleToken("target", "objcopy")})
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
	match, ok, err := metadata.compactKbuildRuleForProfile(profile, "usr/initramfs_inc_data")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || match.command != "copy" {
		t.Fatalf("selected match = %#v, found=%t; want source-evaluated copy command", match, ok)
	}
	if got := kbuildRecipeCommandExpressions(profile.Rules[0].Recipe); !slices.Equal(got, []string{"$(compress-y)"}) {
		t.Fatalf("if_changed expressions = %q, want $(compress-y)", got)
	}
}

func TestGenericKbuildRuleDoesNotConflictWithItsOtherMatchingTargetPattern(t *testing.T) {
	const target = "arch/x86/tools/relocs_32.o"
	profile := mustCompactKbuildProfileForTest(t, "build:arch/x86/tools", "scripts/Makefile.build", "arch/x86/tools", `
$(obj)/relocs_%.o $(obj)/relocs%2.o: FORCE
	$(call if_changed,$*)
`, map[string]string{"obj": "arch/x86/tools"})
	metadata := &CompactMetadata{Config: CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
	}}

	match, ok, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("no evaluated rule found for %q", target)
	}
	if got, want := match.command, "32"; got != want {
		t.Fatalf("selected command = %q, want %q from the first equally specific target pattern", got, want)
	}
}

func TestGenericKbuildStaticPatternRulePrecedesImplicitFallback(t *testing.T) {
	const target = "arch/x86/tools/relocs_32.o"
	profile := mustCompactKbuildProfileForTest(t, "build:arch/x86/tools", "scripts/Makefile.build", "arch/x86/tools", `
host-cobjs := relocs_32.o
cmd_target = $(CC) -c -o $@ $<
cmd_host-cobjs = $(HOSTCC) -c -o $@ $<
$(obj)/%.o: $(src)/%.c FORCE
	$(call if_changed_dep,target)
$(host-cobjs): $(obj)/%.o: $(obj)/%.c FORCE
	$(call if_changed_dep,host-cobjs)
`, map[string]string{"obj": "arch/x86/tools", "src": "arch/x86/tools"})
	metadata := &CompactMetadata{Config: CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
	}}

	match, ok, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("no evaluated rule found for %q", target)
	}
	if got, want := match.command, "host-cobjs"; got != want {
		t.Fatalf("selected command = %q, want static-pattern command %q", got, want)
	}
}

func TestGenericKbuildStaticPatternRuleDoesNotEvaluateImplicitFallback(t *testing.T) {
	const target = "arch/x86/tools/relocs_32.o"
	profile := mustCompactKbuildProfileForTest(t, "build:arch/x86/tools", "scripts/Makefile.build", "arch/x86/tools", `
host-cobjs := relocs_32.o
cmd_host-cobjs = $(HOSTCC) -c -o $@ $<
rule_unselected = $(rule_unselected)
if_changed_rule = $(rule_$(1))
$(obj)/%.o: $(src)/%.c FORCE
	$(call if_changed_rule,unselected)
$(host-cobjs): $(obj)/%.o: $(obj)/%.c FORCE
	$(call if_changed_dep,host-cobjs)
`, map[string]string{"obj": "arch/x86/tools", "src": "arch/x86/tools"})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "arch/x86/tools/relocs_32.c")
	metadata := &CompactMetadata{Config: CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
	}}

	match, ok, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("no evaluated rule found for %q", target)
	}
	if got, want := match.command, "host-cobjs"; got != want {
		t.Fatalf("selected command = %q, want static-pattern command %q", got, want)
	}
}

func TestSelectedKbuildImplicitRecipeMergesExplicitPrerequisites(t *testing.T) {
	const target = "demo.o"
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
cmd_compile = $(CC) -c -o $@ $<
demo.o: generated.h
%.o: %.c
	$(call if_changed,compile)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "demo.c", "generated.h")
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}

	match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !found || match.explicit || match.command != "compile" {
		t.Fatalf("selected match = %#v, found=%t; want implicit compile recipe", match, found)
	}
	normal, orderOnly, stem, err := evaluatedKbuildSelectedTargetRuleContext(profile, target, &match)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := normal, []string{"demo.c", "generated.h"}; !slices.Equal(got, want) {
		t.Fatalf("selected implicit context prerequisites = %q, want %q", got, want)
	}
	if len(orderOnly) != 0 || stem != "demo" {
		t.Fatalf("selected implicit context stem=%q order-only=%q, want demo and none", stem, orderOnly)
	}
}

func TestSelectedKbuildContextPreservesDuplicatesForPlusAndDeduplicatesCaret(t *testing.T) {
	const target = "result.out"
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
automatic = plus=[$+] caret=[$^]
cmd_emit = touch $@
result.out: shared.in shared.in extra.in shared.in
	$(call if_changed,emit)
`, map[string]string{
		"LD":      KbuildActionRoleToken("target", "ld"),
		"OBJCOPY": KbuildActionRoleToken("target", "objcopy"),
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "shared.in", "extra.in")
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}

	match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("no evaluated rule found for %q", target)
	}
	normal, orderOnly, stem, err := evaluatedKbuildSelectedTargetRuleContext(profile, target, &match)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := normal, []string{"shared.in", "shared.in", "extra.in", "shared.in"}; !slices.Equal(got, want) {
		t.Fatalf("raw prerequisites = %q, want duplicate-preserving %q", got, want)
	}
	values, err := EvaluateCompactKbuildTarget(profile, target, stem, normal, orderOnly, nil, "automatic")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["automatic"], "plus=[shared.in shared.in extra.in shared.in] caret=[shared.in extra.in]"; got != want {
		t.Fatalf("automatic prerequisite variables = %q, want %q", got, want)
	}
}

func TestSelectedKbuildLastExplicitRecipeWins(t *testing.T) {
	const target = "result.out"
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
cmd_first = touch $@
cmd_second = touch $@
result.out: first.in
	$(call if_changed,first)
result.out: second.in
	$(call if_changed,second)
`, map[string]string{"AWK": KbuildActionRoleToken("target", "awk")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "first.in", "second.in")
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}

	match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !found || match.command != "second" || match.ruleOrder != 1 {
		t.Fatalf("selected match = %#v, found=%t; want last explicit recipe", match, found)
	}
	normal, _, _, err := evaluatedKbuildSelectedTargetRuleContext(profile, target, &match)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := normal, []string{"second.in", "first.in"}; !slices.Equal(got, want) {
		t.Fatalf("merged explicit prerequisites = %q, want selected recipe first %q", got, want)
	}
}

func TestGenericKbuildShellControlFlowUsesHermeticEvaluatedScript(t *testing.T) {
	const (
		target        = ".checked-atomic-arch-fallback.h"
		source        = "include/linux/atomic/atomic-arch-fallback.h"
		visibleHeader = "include/generated/autoconf.h"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:root", "Kbuild", "", `
cmd = @$(if $(cmd_$(1)),set -e; $(cmd_$(1)),:)
make-cmd = $(cmd_$(1))
dot-target = $(dir $@).$(notdir $@)
cmd_and_savecmd = $(cmd); printf '%s\n' 'savedcmd_$@ := $(make-cmd)' > $(dot-target).cmd
if-changed-cond = 1
if_changed = $(if $(if-changed-cond),$(cmd_and_savecmd),@:)
cmd_check_sha1 = if ! $(AWK) 'BEGIN { exit 0 }' </dev/null; then exit 1; fi; if ! command -v sha1sum >/dev/null; then echo missing; exit 0; fi; if [ "$$(sed -n '$$s:// ::p' $<)" != "$$(sed '$$d' $< | sha1sum | sed 's/ .*//')" ]; then exit 1; fi; touch $@
$(obj)/.checked-%: include/linux/atomic/% FORCE
	$(call if_changed,check_sha1)
`, map[string]string{"obj": ".", "AWK": KbuildActionRoleToken(KbuildActionRoleAutoScope, "awk")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	visibleArtifact := CompactKbuildVisibleArtifact{
		Path: visibleHeader, Profile: profile.Name, Target: visibleHeader,
	}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []CompactKbuildVisibleArtifact{visibleArtifact})
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	selectedMatch, selectedFound, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil || !selectedFound {
		t.Fatalf("control-flow command selection: found=%t err=%v", selectedFound, err)
	}
	selectedTemplates, err := EvaluateCompactKbuildCommandTemplates(
		profile, selectedMatch.rule, target, selectedMatch.stem, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range selectedTemplates {
		if strings.Contains(selected.Text, "$(cmd_") {
			t.Fatalf("control-flow wrapper retained an unevaluated command reference: %q", selected.Text)
		}
	}
	headerProducer := ActionPlanNode{
		ID: strings.Repeat("e", 64), Stage: "bootstrap", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "bootstrap", Path: visibleHeader}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}, Nodes: []ActionPlanNode{headerProducer}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("prep", "prep", "sdk")
	builder = withMaterializedInitialObjectTreeArtifactForTest(
		t, builder, visibleArtifact, "bootstrap", "target", "target", headerProducer.ID,
	)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing producer %s", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("node=%#v recipe=%#v, want hermetic script runner", node, recipe)
	}
	if !slices.Contains(recipe.AuxiliaryTools, compactKbuildScriptRuntimeRole) {
		t.Fatalf("auxiliary tools=%q, want script runtime", recipe.AuxiliaryTools)
	}
	if !slices.Contains(recipe.Arguments, compactKbuildScriptRuntimeRole+"=${tool:"+compactKbuildScriptRuntimeRole+"}") {
		t.Fatalf("script arguments=%q, want bound script-runtime contract role", recipe.Arguments)
	}
	if !slices.Contains(recipe.AuxiliaryTools, "awk") {
		t.Fatalf("auxiliary tools=%q, want resolved scope-neutral AWK", recipe.AuxiliaryTools)
	}
	if got := recipe.WorkingInputs["source:prerequisite:00000000"]; got != source {
		t.Fatalf("working source path=%q, want %q", got, source)
	}
	if got, want := len(actionPlanNodeInputSetEntriesForTest(t, plan, node)), 1; got != want {
		t.Fatalf("hermetic fallback persistent inputs=%#v, want exact initial object-tree frontier", actionPlanNodeInputSetEntriesForTest(t, plan, node))
	}
	visibleInput, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, node, visibleHeader)
	wantVisibleInput := ActionPlanInputSetEntry{
		Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: visibleHeader},
		ProducerID: headerProducer.ID,
	}
	if !found || visibleInput != wantVisibleInput {
		t.Fatalf("hermetic fallback visible frontier input = (%#v, %t), want %#v", visibleInput, found, wantVisibleInput)
	}
	if got := recipe.WorkingOutputs["00000000"]; got != target {
		t.Fatalf("working output=%q, want %q", got, target)
	}
	encodedIndex := slices.Index(recipe.Arguments, "-script_content_base64")
	if encodedIndex < 0 || encodedIndex+1 == len(recipe.Arguments) {
		t.Fatalf("script arguments=%q", recipe.Arguments)
	}
	decoded, err := base64.StdEncoding.DecodeString(recipe.Arguments[encodedIndex+1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decoded), "awk 'BEGIN { exit 0 }'") || strings.Contains(string(decoded), kbuildActionRoleTokenPrefix) ||
		strings.Contains(string(decoded), "@if") || strings.Contains(string(decoded), "$(1)") || !strings.Contains(string(decoded), "savedcmd_") ||
		!strings.Contains(string(decoded), "sha1sum") || !strings.Contains(string(decoded), "touch "+target) {
		t.Fatalf("evaluated script=%q", decoded)
	}
}

func TestGenericKbuildBraceGroupRedirectUsesHermeticEvaluatedScript(t *testing.T) {
	const target = "arch/arm64/boot/dts/actions/dtbs-list"
	profile := mustCompactKbuildProfileForTest(t, "build:arch/arm64/boot/dts/actions", "scripts/Makefile.build", "arch/arm64/boot/dts/actions", `
real-prereqs = $(objtree)/arch/arm64/boot/dts/vendor/dtbs-list $(objtree)/arch/arm64/boot/dts/actions/example.dtb
cmd_gen_order = { $(foreach m, $(real-prereqs), $(if $(filter %/$(notdir $@), $m), cat $m, echo $m);) :; } > $@
arch/arm64/boot/dts/actions/dtbs-list: $(real-prereqs) FORCE
	$(call if_changed,gen_order)
`, map[string]string{"objtree": "__LINUX_BZL_OBJECT_TREE__"})
	profile = compactKbuildProfileWithSourcesForTest(
		t, profile,
		"arch/arm64/boot/dts/vendor/dtbs-list",
		"arch/arm64/boot/dts/actions/example.dtb",
	)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "arch/arm64/boot/dts/actions",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("brace-group node count = %d, want one hermetic script: %#v", got, plan.Nodes)
	}
	node := plan.Nodes[0]
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole ||
		recipe.WorkingOutputs["00000000"] != target || recipe.ExecutionDirectory != "arch/arm64/boot/dts/actions" {
		t.Fatalf("brace-group node=%#v recipe=%#v, want hermetic target-producing script", node, recipe)
	}
	workingInputs := map[string]bool{}
	for _, pathname := range recipe.WorkingInputs {
		workingInputs[pathname] = true
	}
	for _, want := range []string{
		"arch/arm64/boot/dts/vendor/dtbs-list",
		"arch/arm64/boot/dts/actions/example.dtb",
	} {
		if !workingInputs[want] {
			t.Errorf("brace-group working inputs omit %q: %#v", want, recipe.WorkingInputs)
		}
	}
	if got, want := len(node.Sources), 2; got != want {
		t.Errorf("brace-group source edges = %d, want %d: %#v", got, want, node.Sources)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	for _, want := range []string{"{ cat ", "vendor/dtbs-list", "; echo ", "example.dtb", "; :; } > ", "dtbs-list"} {
		if !strings.Contains(script, want) {
			t.Errorf("brace-group script omits %q: %q", want, script)
		}
	}
}

func TestGenericKbuildCmdCallSelectsSourceDefinedCommand(t *testing.T) {
	const target = "arch/x86/include/generated/asm/early_ioremap.h"
	const source = "include/asm-generic/early_ioremap.h"
	profile := mustCompactKbuildProfileForTest(t, "driver:scripts/Makefile.asm-headers", "scripts/Makefile.asm-headers", "", `
cmd_wrap = echo "\#include <asm-generic/$*.h>" > $@
$(obj)/%.h: $(generic)/%.h
	$(call cmd,wrap)
`, map[string]string{
		"obj":     "arch/x86/include/generated/asm",
		"generic": "include/asm-generic",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
	match, ok, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || match.command != "wrap" {
		t.Fatalf("match=%#v found=%t, want source-defined cmd_wrap", match, ok)
	}
	root := profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	if !filepath.IsAbs(root) {
		t.Fatalf("source fixture requires an absolute temporary root, got %q", root)
	}
	packageSource, err := filepath.Abs(source)
	if err != nil {
		t.Fatal(err)
	}
	fixtureSource := filepath.Join(root, source)
	if fixtureSource == packageSource {
		t.Fatal("source fixture would write into the package directory")
	}
	mustWriteSource(t, root, source, "#define EARLY_IOREMAP 1\n")
	if contents, err := os.ReadFile(fixtureSource); err != nil || string(contents) != "#define EARLY_IOREMAP 1\n" {
		t.Fatalf("temporary source fixture contents=%q, error=%v", contents, err)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	baseProducer, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "actionfile", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "metadata", Path: ".captures/asm-wrapper-base"}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-line", "base", "-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	builder, err = builder.forObservedOutputs(target, []compactKbuildObservedOutput{{
		output: ActionPlanOutput{Tree: "metadata", Path: ".captures/asm-wrapper"},
		path:   ".vmlinux.export.c",
		baseInputs: []compactKbuildRuleInput{{
			path: ".captures/asm-wrapper-base", producer: baseProducer, slot: 0,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	producer, err := builder.buildCommandTemplate(target, match, []compactKbuildRuleInput{{
		path: source, sourceID: sourceID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing asm wrapper producer %q", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	wantContents := base64.StdEncoding.EncodeToString([]byte("#include <asm-generic/early_ioremap.h>\n"))
	wantArguments := []string{
		"-content_base64", wantContents,
		"-out", "${output:00000000}",
	}
	if node.Tool != "actionfile" || recipe.Tool != "actionfile" ||
		!slices.Equal(recipe.Arguments, wantArguments) {
		t.Fatalf("asm wrapper node=%#v recipe=%#v, want literal actionfile lowering", node, recipe)
	}
	if len(recipe.Environment) != 0 || len(recipe.Trees) != 0 || len(node.Trees) != 0 ||
		len(recipe.ObservedOutputs) != 0 || len(recipe.ObservedOutputBases) != 0 || len(node.Outputs) != 1 {
		t.Fatalf("literal asm wrapper retained process/tree/observation state: node=%#v recipe=%#v", node, recipe)
	}
	for _, input := range node.Inputs {
		if input.ProducerID == baseProducer {
			t.Fatalf("literal asm wrapper retained unrelated observed-state input: %#v", node.Inputs)
		}
	}
	var stateNode ActionPlanNode
	for _, candidate := range plan.Nodes {
		for _, output := range candidate.Outputs {
			if output.ObservedPath == ".vmlinux.export.c" {
				stateNode = candidate
			}
		}
	}
	if stateNode.ID == "" || stateNode.ID == producer || len(stateNode.Outputs) != 2 ||
		!strings.HasPrefix(stateNode.Outputs[0].Path, compactKbuildSideOutputStateDirectory+"/transparent/") {
		t.Fatalf("literal asm wrapper transparent state node=%#v, plan=%#v", stateNode, plan.Nodes)
	}
	stateRecipe := plan.Recipes[stateNode.Recipe]
	stateBinding := planOrdinal(1)
	if !slices.Equal(stateRecipe.Arguments, []string{"-content_base64", "", "-out", "${output:00000000}"}) ||
		stateRecipe.ObservedOutputs[stateBinding] != ".vmlinux.export.c" ||
		len(stateRecipe.ObservedOutputBases[stateBinding]) != 1 {
		t.Fatalf("literal asm wrapper transparent state recipe=%#v", stateRecipe)
	}
	baseBinding := stateRecipe.ObservedOutputBases[stateBinding][0]
	baseIndex := slices.Index(stateRecipe.Inputs, baseBinding)
	if baseIndex < 0 || baseIndex >= len(stateNode.Inputs) ||
		stateNode.Inputs[baseIndex].ProducerID != baseProducer || stateNode.Inputs[baseIndex].Slot != 0 {
		t.Fatalf("literal asm wrapper transparent state base=%q node=%#v recipe=%#v", baseBinding, stateNode, stateRecipe)
	}
	if err := stateRecipe.Validate(); err != nil {
		t.Fatalf("literal asm wrapper transparent state recipe is invalid: %v", err)
	}
	if err := recipe.Validate(); err != nil {
		t.Fatalf("literal asm wrapper recipe is invalid: %v", err)
	}
}

func TestGenericKbuildExtendedIfChangedAndFollowingCommandLowerInOrder(t *testing.T) {
	const target = "drivers/demo.ko"
	profile := mustCompactKbuildProfileForTest(t, "modfinal", "scripts/Makefile.modfinal", "", `
LD = /selected/ld
OBJCOPY = /selected/objcopy
cmd_ld_ko_o = $(LD) -r -o $@ $<
cmd_post_ko = $(OBJCOPY) --strip-debug $@
cmd_disabled_check =
drivers/demo.ko: drivers/demo.o FORCE
	+$(call if_changed_except,ld_ko_o,vmlinux)
	+$(if y,$(call cmd,post_ko))
	+$(call cmd,disabled_check)
`, map[string]string{
		"LD":      KbuildActionRoleToken("target", "ld"),
		"OBJCOPY": KbuildActionRoleToken("target", "objcopy"),
	})
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	inputRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "cc",
		Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	inputProducer, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "cc", Product: "modules",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: "drivers/demo.o"}},
	}, inputRecipe)
	if err != nil {
		t.Fatal(err)
	}

	match, ok, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("extended if_changed rule was not selected")
	}
	if got, want := match.commandSequence(), []string{"ld_ko_o", "post_ko"}; !slices.Equal(got, want) {
		t.Fatalf("selected command sequence = %q, want %q", got, want)
	}

	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("target", "modules", "modules")
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 3; got != want {
		t.Fatalf("node count = %d, want input + two source-recipe commands: %#v", got, plan.Nodes)
	}
	link := plan.Nodes[1]
	if link.Tool != "ld" || link.Inputs[0].ProducerID != inputProducer || link.Outputs[0].Path != target ||
		!strings.HasPrefix(link.Outputs[0].ArtifactPath, ".linux-bzl-intermediate/") ||
		actionPlanOutputIsCanonical(link.Outputs[0]) {
		t.Fatalf("link node = %#v", link)
	}
	post := plan.Nodes[2]
	if post.ID != producer || post.Tool != "objcopy" || post.Outputs[0] != (ActionPlanOutput{Tree: "modules", Path: target}) {
		t.Fatalf("postprocess node = %#v", post)
	}
	dependsOnLink := false
	for _, input := range post.Inputs {
		if input.ProducerID == link.ID {
			dependsOnLink = true
		}
	}
	if !dependsOnLink {
		t.Fatalf("postprocess inputs = %#v, want link producer %s", post.Inputs, link.ID)
	}
	postRecipe := plan.Recipes[post.Recipe]
	stagesLinkedTarget := false
	for _, workingPath := range postRecipe.WorkingInputs {
		if workingPath == target {
			stagesLinkedTarget = true
		}
	}
	if !stagesLinkedTarget {
		t.Fatalf("postprocess working inputs = %#v, want linked target %q", postRecipe.WorkingInputs, target)
	}
}

func TestGenericKbuildTargetSpecificLeadingSemicolonKeepsBindgenPostprocessor(t *testing.T) {
	const (
		target = "rust/bindings/bindings_helpers_generated.rs"
		source = "rust/helpers/helpers.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:rust", "rust/Makefile", "rust", `
bindgen_target_flags =
bindgen_target_cflags =
bindgen_target_extra =
cmd_bindgen = $(BINDGEN) $< $(bindgen_target_flags) -o $@ -- $(bindgen_target_cflags) $(bindgen_target_extra)
$(obj)/bindings/bindings_helpers_generated.rs: private bindgen_target_flags = --blocklist-type '.*' --allowlist-function 'rust_helper_.*'
$(obj)/bindings/bindings_helpers_generated.rs: private bindgen_target_cflags = -I$(objtree)/$(obj)
$(obj)/bindings/bindings_helpers_generated.rs: private bindgen_target_extra = ; sed -Ei 's/pub fn rust_helper_([a-zA-Z0-9_]*)/#[link_name="rust_helper_\1"]\n    pub fn \1/g' $@
$(obj)/bindings/bindings_helpers_generated.rs: $(src)/helpers/helpers.c FORCE
	$(call if_changed_dep,bindgen)
`, map[string]string{
		"BINDGEN": KbuildActionRoleToken("target", "bindgen"),
		"obj":     "rust",
		"objtree": "",
		"src":     "rust",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "rust",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: append(
			slices.Clone(testConfiguredScopedActionRoles),
			KbuildActionRoleRef{Scope: "target", Role: "bindgen"},
		),
		Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, lookupTarget: target, command: "bindgen",
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 2; got != want {
		t.Fatalf("node count = %d, want bindgen plus exact postprocess action: %#v", got, plan.Nodes)
	}
	bindgen, postprocess := plan.Nodes[0], plan.Nodes[1]
	bindgenRecipe, postprocessRecipe := plan.Recipes[bindgen.Recipe], plan.Recipes[postprocess.Recipe]
	if bindgen.Tool != "bindgen" || bindgenRecipe.Tool != "bindgen" ||
		bindgen.Outputs[0].Path != target || actionPlanOutputIsCanonical(bindgen.Outputs[0]) {
		t.Fatalf("bindgen intermediate = %#v recipe = %#v", bindgen, bindgenRecipe)
	}
	if producer != postprocess.ID || postprocess.Tool != compactKbuildScriptRuntimeRole ||
		postprocessRecipe.Tool != compactKbuildScriptRuntimeRole ||
		postprocess.Outputs[0] != (ActionPlanOutput{Tree: "objects", Path: target}) ||
		!slices.ContainsFunc(postprocess.Inputs, func(input ActionPlanNodeEdge) bool {
			return input.ProducerID == bindgen.ID && input.Slot == 0
		}) {
		t.Fatalf("bindgen postprocessor = %#v recipe = %#v", postprocess, postprocessRecipe)
	}
	bindgenArguments := strings.Join(bindgenRecipe.Arguments, " ")
	for _, want := range []string{"--allowlist-function", "rust_helper_.*", "-I${tree:prep}/rust"} {
		if !strings.Contains(bindgenArguments, want) {
			t.Errorf("bindgen arguments omit %q: %q", want, bindgenRecipe.Arguments)
		}
	}
	postprocessArguments := strings.Join(postprocessRecipe.Arguments, " ")
	for _, want := range []string{"sed -Ei", `#[link_name="rust_helper_\1"]`, "pub fn \\1"} {
		if !strings.Contains(postprocessArguments, want) {
			t.Errorf("bindgen postprocessor arguments omit %q: %q", want, postprocessRecipe.Arguments)
		}
	}
	stagesIntermediate := false
	for _, pathname := range postprocessRecipe.WorkingInputs {
		if pathname == target {
			stagesIntermediate = true
		}
	}
	if !stagesIntermediate {
		t.Fatalf("bindgen postprocessor does not stage %q: %#v", target, postprocessRecipe.WorkingInputs)
	}
}

func TestGenericKbuildSelectionUsesOnlyItsExactProfile(t *testing.T) {
	const (
		target = "drivers/demo.o"
		source = "drivers/demo.c"
	)
	first := mustCompactKbuildProfileForTest(t, "build:drivers#first", "scripts/Makefile.build", "", `
cmd_cc_o_c = $(CC) -DFIRST_PROFILE -c -o $@ $<
drivers/%.o: drivers/%.c FORCE
	$(call if_changed,cc_o_c)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
	first = compactKbuildProfileWithSourcesForTest(t, first, source)
	second := mustCompactKbuildProfileForTest(t, "build:drivers#second", "scripts/Makefile.build", "", `
cmd_cc_o_c = $(CC) -DPOISON_PROFILE -c -o $@ $<
drivers/%.o: drivers/%.c FORCE
	$(call if_changed,cc_o_c)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{first, second}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(first)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing producer %s", producer)
	}
	arguments := strings.Join(plan.Recipes[node.Recipe].Arguments, " ")
	if !strings.Contains(arguments, "-DFIRST_PROFILE") || strings.Contains(arguments, "POISON_PROFILE") {
		t.Fatalf("exact-profile arguments = %q", arguments)
	}
	poisonBuilder := newCompactKbuildRulePlanBuilder(metadata, &ActionPlan{Recipes: map[string]ActionRecipe{}}).forProfile(second)
	if _, err := poisonBuilder.build(target); err == nil || !strings.Contains(err.Error(), "has no source evidence") {
		t.Fatalf("profile without %q source evidence error = %v", source, err)
	}
}

func TestGenericKbuildRecipeLowersRedirectedCatToDeclaredCopy(t *testing.T) {
	const (
		target = "usr/initramfs_inc_data"
		source = "usr/initramfs_data.cpio"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:usr", "scripts/Makefile.build", "usr", `
cmd_copy = cat $< > $@
`, nil)
	metadata := &CompactMetadata{actionRoles: testConfiguredScopedActionRoles}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forOutput("prep", "prep", "sdk")
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "initramfs_inc_data", command: "copy",
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("copy producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != "actionfile" || node.Kind != "copy" || recipe.Tool != "actionfile" {
		t.Fatalf("copy node = %#v, recipe = %#v", node, recipe)
	}
	if got, want := recipe.Arguments, []string{"-input", "${source:object:00000000}", "-out", "${output:00000000}"}; !slices.Equal(got, want) {
		t.Fatalf("copy arguments = %#v, want %#v", got, want)
	}
}

func TestGenericKbuildRuleViabilityUsesDeclaredSourceTree(t *testing.T) {
	const (
		directory = "arch/x86/boot/startup"
		target    = directory + "/gdt_idt.o"
	)
	root := t.TempDir()
	source := filepath.Join(root, filepath.FromSlash(directory), "gdt_idt.c")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("int gdt_idt;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
cmd_cc_o_c = $(CC) -c -o $@ $<
cmd_as_o_S = $(CC) -c -o $@ $<
$(obj)/%.o: $(obj)/%.c FORCE
	$(call if_changed_rule,cc_o_c)
$(obj)/%.o: $(obj)/%.S FORCE
	$(call if_changed_rule,as_o_S)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc"), "obj": directory})
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}
	match, ok, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || match.command != "cc_o_c" {
		t.Fatalf("selected match = %#v, found=%t; want the rule backed by declared gdt_idt.c", match, ok)
	}
}

func TestGenericKbuildRuleViabilitySelectsAssemblyBeforeMergingExplicitPrerequisites(t *testing.T) {
	const (
		directory = "arch/arm64/kernel"
		target    = directory + "/vdso-wrap.o"
		assembly  = directory + "/vdso-wrap.S"
		vdso      = directory + "/vdso/vdso.so"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
cmd_cc_o_c = $(CC) -c -o $@ $<
cmd_as_o_S = $(CC) -c -o $@ $<
$(obj)/%.o: $(obj)/%.c FORCE
	$(call if_changed_rule,cc_o_c)
$(obj)/%.o: $(obj)/%.S FORCE
	$(call if_changed_rule,as_o_S)
$(obj)/vdso-wrap.o: $(obj)/vdso/vdso.so
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc"), "obj": directory})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, assembly)
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}

	match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !found || match.command != "as_o_S" {
		t.Fatalf("selected match = %#v, found=%t; want assembly rule backed by %q", match, found, assembly)
	}
	normal, _, _, err := evaluatedKbuildSelectedTargetRuleContext(profile, target, &match)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(normal, assembly) || !slices.Contains(normal, vdso) {
		t.Fatalf("post-selection merged prerequisites = %#v, want assembly source %q and explicit prerequisite %q", normal, assembly, vdso)
	}

	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	vdsoProducer := appendSelectionScopeTestOutput(t, plan, "target", "objects", vdso)
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok || node.Tool != "cc" || !slices.ContainsFunc(node.Inputs, func(input ActionPlanNodeEdge) bool {
		return input.ProducerID == vdsoProducer
	}) {
		t.Fatalf("vdso wrapper node = %#v, want assembly compile with generated vdso input from %q", node, vdsoProducer)
	}
}

func TestGenericKbuildRuleViabilityDoesNotReuseImplicitRuleRecursively(t *testing.T) {
	const (
		directory = "arch/x86/tools"
		target    = directory + "/relocs"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
cmd_shipped = cp $< $@
cmd_source = cp $< $@
$(obj)/%: $(obj)/%_shipped FORCE
	$(call if_changed,shipped)
$(obj)/%: $(obj)/%.c FORCE
	$(call if_changed,source)
`, map[string]string{"obj": directory})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, directory+"/relocs.c")
	metadata := &CompactMetadata{Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}}}

	match, ok, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || match.command != "source" {
		t.Fatalf("selected match = %#v, found=%t; want source after rejecting recursive %%_shipped fallback", match, ok)
	}
}

func TestCompactKbuildProfileTargetPathStripsInvocationTreeMarkers(t *testing.T) {
	profile := CompactKbuildProfile{Directory: "arch/x86/kernel"}
	for input, want := range map[string]string{
		"__LINUX_BZL_SOURCE_TREE__/arch/x86/kernel/vmlinux.lds.S":                       "arch/x86/kernel/vmlinux.lds.S",
		"__LINUX_BZL_OBJECT_TREE__/arch/x86/kernel/vmlinux.lds":                         "arch/x86/kernel/vmlinux.lds",
		"__LINUX_BZL_SOURCE_TREE__/arch/x86/kernel/cpu/../../include/asm/cpufeatures.h": "arch/x86/include/asm/cpufeatures.h",
		"${tree:prehost}/scripts/basic/fixdep":                                          "scripts/basic/fixdep",
		"${tree:bootstrap}/arch/x86/kernel/asm-offsets.s":                               "arch/x86/kernel/asm-offsets.s",
		"${tree:host}/scripts/mod/modpost":                                              "scripts/mod/modpost",
		"${tree:prep}/include/generated/autoconf.h":                                     "include/generated/autoconf.h",
	} {
		if got := compactKbuildProfileTargetPath(profile, input); got != want {
			t.Errorf("compactKbuildProfileTargetPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCompactKbuildProfileTargetPathKeepsAbsoluteRootMappingRootRelative(t *testing.T) {
	root := t.TempDir()
	profile := mustCompactKbuildProfileForTest(t, "syscalls", "scripts/Makefile.build", "arch/x86/entry/syscalls", "", nil)
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
	input := filepath.Join(root, "scripts", "syscallhdr.sh")
	if got, want := compactKbuildProfileTargetPath(profile, input), "scripts/syscallhdr.sh"; got != want {
		t.Fatalf("compactKbuildProfileTargetPath(%q) = %q, want root-relative %q", input, got, want)
	}
}

func TestCompactKbuildProfileTargetPathUsesSourceAncestryForRelativeScope(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "scripts/syscallhdr.sh", "#!/bin/sh\n")
	mustWriteSource(t, root, "arch/x86/entry/syscalls/syscall_32.tbl", "0 i386 restart_syscall\n")
	if err := os.MkdirAll(filepath.Join(root, "arch/x86/include"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := mustCompactKbuildProfileForTest(t, "syscalls", "scripts/Makefile.build", "arch/x86/entry/syscalls", "", nil)
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
	for input, want := range map[string]string{
		"scripts/syscallhdr.sh":                    "scripts/syscallhdr.sh",
		"arch/x86/include/generated/uapi/unistd.h": "arch/x86/include/generated/uapi/unistd.h",
		"syscall_32.tbl":                           "arch/x86/entry/syscalls/syscall_32.tbl",
	} {
		if got := compactKbuildProfileTargetPath(profile, input); got != want {
			t.Errorf("compactKbuildProfileTargetPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCompactKbuildProfileTargetPathUsesTypedInvocationWorkingDirectory(t *testing.T) {
	realRoot := t.TempDir()
	mustWriteSource(t, realRoot, "arch/x86/Makefile", "# root arch evidence must not win\n")
	mustWriteSource(t, realRoot, "tools/objtool/sync-check.sh", "#!/bin/sh\n")
	mustWriteSource(t, realRoot, "tools/objtool/arch/x86/lib/inat.c", "int inat;\n")
	aliasParent := t.TempDir()
	aliasRoot := filepath.Join(aliasParent, "linux")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatal(err)
	}

	profile := mustCompactKbuildProfileForTest(t, "objtool", "tools/build/Makefile.build", "tools/objtool", "", nil)
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": aliasRoot}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "tools/objtool",
	}); err != nil {
		t.Fatal(err)
	}

	for input, want := range map[string]string{
		"arch/x86/lib/inat-tables.c":       "tools/objtool/arch/x86/lib/inat-tables.c",
		"tools/objtool/already-expanded.o": "tools/objtool/already-expanded.o",
		"$(obj)/invocation-local.o":        "tools/objtool/invocation-local.o",
	} {
		if got := compactKbuildProfileTargetPath(profile, input); got != want {
			t.Errorf("compactKbuildProfileTargetPath(%q) = %q, want %q", input, got, want)
		}
	}
	if got, source, ok := compactKbuildProfileCommandPath(profile, "./sync-check.sh"); !ok || !source || got != "tools/objtool/sync-check.sh" {
		t.Fatalf("compactKbuildProfileCommandPath(./sync-check.sh) = (%q,%t,%t), want invocation-relative source script", got, source, ok)
	}
}

func TestCompactKbuildProfileTargetPathKeepsTypedRootInvocationRooted(t *testing.T) {
	root := t.TempDir()
	profile := mustCompactKbuildProfileForTest(t, "build:arch/x86/kernel", "scripts/Makefile.build", "arch/x86/kernel", "", nil)
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}

	for input, want := range map[string]string{
		"arch/x86/kernel/already-expanded.o": "arch/x86/kernel/already-expanded.o",
		"unmarked-root-target":               "unmarked-root-target",
		"$(obj)/invocation-local.o":          "arch/x86/kernel/invocation-local.o",
	} {
		if got := compactKbuildProfileTargetPath(profile, input); got != want {
			t.Errorf("compactKbuildProfileTargetPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCompactKbuildProfileTargetPathUsesDirectoryWithoutEvaluator(t *testing.T) {
	profile := CompactKbuildProfile{Directory: "tools/objtool"}
	if got, want := compactKbuildProfileTargetPath(profile, "arch/x86/lib/inat-tables.c"), "tools/objtool/arch/x86/lib/inat-tables.c"; got != want {
		t.Fatalf("compactKbuildProfileTargetPath without evaluator = %q, want %q", got, want)
	}
}

func TestGenericKbuildRecipeErasesValidatedOutputDirectorySetup(t *testing.T) {
	metadata, target := compactGenericRecipeMetadataForTest(
		t, `mkdir -p generated/ ; tools/filter $< > $@`, ":", "generated/result.h",
	)
	plan := compactGenericRecipePlanForTest()
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 2; got != want {
		t.Fatalf("node count=%d, want %d: %#v", got, want, plan.Nodes)
	}
	result := plan.Nodes[1]
	if result.Outputs[0].Path != target || plan.Recipes[result.Recipe].Stdout != "00000000" {
		t.Fatalf("result=%#v recipe=%#v", result, plan.Recipes[result.Recipe])
	}
}

func TestKbuildRecipeDirectorySetupCanonicalizesEvaluatedTreePath(t *testing.T) {
	command := compactKbuildRecipeCommand{
		program:   "mkdir",
		arguments: []string{"-p", "${tree:prep}/tools/bpf/resolve_btfids//libsubcmd"},
	}
	setup, err := compactKbuildRecipeDirectorySetup(command)
	if err != nil {
		t.Fatal(err)
	}
	if !setup {
		t.Fatal("evaluated mkdir was not recognized as directory setup")
	}
}

func TestEvaluatedKbuildInstallDirectoryRecipeIsOrderingOnly(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "install", "scripts/install.mk", "", `
generated/include:
	install -d $@
`, nil)
	match := compactKbuildRuleMatch{
		profile: profile, rule: profile.Rules[0], lookupTarget: "generated/include", explicit: true, resolved: true,
	}
	action, directorySetup, err := evaluatedKbuildDirectRecipeEffects("generated/include", match)
	if err != nil {
		t.Fatal(err)
	}
	if action || !directorySetup {
		t.Fatalf("install directory effects = action:%t directory:%t", action, directorySetup)
	}
}

func TestGenericKbuildRecipeRunsThinArchiveInTypedWorkingTree(t *testing.T) {
	const target = "drivers/example/built-in.a"
	profile := mustCompactKbuildProfileForTest(t, "build:drivers/example", "scripts/Makefile.lib", "drivers/example", `
cmd_ar = rm -f $@; $(AR) cDPrsT $@ $(real-prereqs)
`, map[string]string{"AR": KbuildActionRoleToken("target", "ar"), "real-prereqs": "drivers/example/one.o drivers/example/two.o"})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	inputs := []compactKbuildRuleInput{}
	for _, input := range []string{"drivers/example/one.o", "drivers/example/two.o"} {
		node := ActionPlanNode{
			Stage: "target", Kind: "compile", Tool: "cc", Product: "vmlinux",
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: input}},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
			Arguments: []string{"-c", "-o", "${output:00000000}"}, Outputs: []string{"00000000"},
		}
		producer, err := appendActionPlanNode(plan, node, recipe)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, compactKbuildRuleInput{path: input, producer: producer})
	}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "built-in", command: "ar",
	}, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 3; got != want {
		t.Fatalf("node count=%d, want two inputs + one archive: %#v", got, plan.Nodes)
	}
	archive, ok := compactKbuildPlanNode(plan, producer)
	if !ok || archive.Kind != "generate" || archive.Tool != compactKbuildScriptRunnerRole || archive.Outputs[0].Path != target {
		t.Fatalf("archive=%#v", archive)
	}
	recipe := plan.Recipes[archive.Recipe]
	if recipe.ExecutionDirectory != "" || len(archive.Inputs) != 2 || len(recipe.WorkingInputs) != 2 {
		t.Fatalf("archive inputs=%#v working=%#v", archive.Inputs, recipe.WorkingInputs)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	want := "rm -f " + target + "; ar cDPrsT " + target + " drivers/example/one.o drivers/example/two.o"
	if !strings.Contains(script, want) || strings.Contains(script, "${input:") {
		t.Fatalf("typed-cwd archive script=%q, want %q without physical input placeholders", script, want)
	}

	if _, err := exec.LookPath("ar"); err != nil {
		t.Skipf("ar is required for thin-archive integration coverage")
	}
	archives := make([][]byte, 0, 2)
	for run := 0; run < 2; run++ {
		private := t.TempDir()
		for _, member := range []string{"drivers/example/one.o", "drivers/example/two.o"} {
			mustWriteSource(t, private, member, fmt.Sprintf("member-%d-%s", run, member))
		}
		command := exec.Command("/bin/sh", "-c", script)
		command.Dir = private
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("execute typed-cwd thin archive: %v\n%s", err, output)
		}
		archiveBytes, err := os.ReadFile(filepath.Join(private, filepath.FromSlash(target)))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(archiveBytes, []byte(private)) {
			t.Fatalf("thin archive contains private root %q: %q", private, archiveBytes)
		}
		for _, member := range []string{"one.o", "two.o"} {
			if !bytes.Contains(archiveBytes, []byte(member)) {
				t.Fatalf("thin archive omits stable member %q: %q", member, archiveBytes)
			}
		}
		archives = append(archives, archiveBytes)
	}
	if !bytes.Equal(archives[0], archives[1]) {
		t.Fatalf("thin archive bytes depend on private working root")
	}
}

func TestGeneratedArchiveWrapperRunsInTypedWorkingTree(t *testing.T) {
	const (
		directory = "drivers/example"
		target    = directory + "/built-in.a"
		wrapper   = "scripts/ar-wrapper"
		object    = directory + "/one.o"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
real-prereqs = $(filter-out FORCE,$^)
cmd_ar = $(AR_WRAPPER) $@ $(real-prereqs)
$(obj)/built-in.a: $(obj)/one.o FORCE
	$(call if_changed,ar)
`, map[string]string{
		"AR_WRAPPER": "__LINUX_BZL_OBJECT_TREE__/" + wrapper,
		"obj":        directory,
	})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	seed := func(stage, kind, tool, output string) string {
		node := ActionPlanNode{
			Stage: stage, Kind: kind, Tool: tool, Product: "vmlinux",
			Outputs: []ActionPlanOutput{{Tree: map[string]string{"host": "host", "target": "objects"}[stage], Path: output}},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: kind, Tool: tool,
			Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
		}
		producer, err := appendActionPlanNode(plan, node, recipe)
		if err != nil {
			t.Fatal(err)
		}
		return producer
	}
	wrapperProducer := seed("host", "generate", "actionfile", wrapper)
	objectProducer := seed("target", "compile", "cc", object)
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	match, matched, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil || !matched {
		t.Fatalf("generated wrapper rule match=(%#v,%t,%v)", match, matched, err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, match, []compactKbuildRuleInput{{path: object, producer: objectProducer}})
	if err != nil {
		t.Fatal(err)
	}
	archive, ok := compactKbuildPlanNode(plan, producer)
	if !ok || archive.Tool != "generated" {
		t.Fatalf("generated wrapper archive=%#v, want one typed generated action", archive)
	}
	recipe := plan.Recipes[archive.Recipe]
	if recipe.ExecutionDirectory != directory || len(recipe.ExecutableInputs) != 1 || !slices.ContainsFunc(archive.Inputs, func(input ActionPlanNodeEdge) bool {
		return input.ProducerID == wrapperProducer
	}) {
		t.Fatalf("generated wrapper recipe=%#v, want typed cwd and executable producer %q", recipe, wrapperProducer)
	}
	wantArguments := []string{"../../" + target, "../../" + object}
	if !slices.Equal(recipe.Arguments, wantArguments) || recipe.WorkingOutputs["00000000"] != target {
		t.Fatalf("generated wrapper arguments=%q outputs=%q, want stable paths %q", recipe.Arguments, recipe.WorkingOutputs, wantArguments)
	}
	if strings.Contains(strings.Join(recipe.Arguments, " "), "${input:") || strings.Contains(strings.Join(recipe.Arguments, " "), "kbuild-command-") {
		t.Fatalf("generated wrapper exposes action-private paths: %q", recipe.Arguments)
	}
}

func TestSourceArchiveWrapperRunsWithInspectedEnvironmentInTypedWorkingTree(t *testing.T) {
	const (
		directory = "drivers/example"
		target    = directory + "/built-in.a"
		wrapper   = "scripts/ar-wrapper"
		object    = directory + "/one.o"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
cmd_ar = $(AR_WRAPPER) $@ $(real-prereqs)
`, map[string]string{
		"AR":           KbuildActionRoleToken("target", "ar"),
		"AR_WRAPPER":   "__LINUX_BZL_SOURCE_TREE__/" + wrapper,
		"real-prereqs": object,
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, wrapper)
	root := profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	mustWriteSource(t, root, wrapper, "#!/bin/sh\nexec \"$AR\" rcT \"$@\"\n")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	objectProducer, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "compile", Tool: "cc", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: object}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "built-in", command: "ar",
	}, []compactKbuildRuleInput{{path: object, producer: objectProducer}})
	if err != nil {
		t.Fatal(err)
	}
	archive, ok := compactKbuildPlanNode(plan, producer)
	if !ok || archive.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("source wrapper archive=%#v", archive)
	}
	recipe := plan.Recipes[archive.Recipe]
	if recipe.ExecutionDirectory != directory || recipe.Environment["AR"] != "ar" || !slices.Contains(recipe.AuxiliaryTools, "ar") {
		t.Fatalf("source wrapper recipe=%#v, want inspected AR capability and typed cwd", recipe)
	}
	delimiter := slices.Index(recipe.Arguments, "--")
	if delimiter < 0 {
		t.Fatalf("source wrapper recipe has no script argument delimiter: %#v", recipe.Arguments)
	}
	arguments := recipe.Arguments[delimiter+1:]
	want := []string{"../../" + target, "../../" + object}
	if !slices.Equal(arguments, want) {
		t.Fatalf("source wrapper arguments=%q, want stable typed paths %q", arguments, want)
	}
	if strings.Contains(strings.Join(arguments, " "), "${input:") || strings.Contains(strings.Join(sortedStringMapValues(recipe.Environment), " "), "${work:root}") {
		t.Fatalf("source wrapper retains action-private paths: arguments=%q environment=%q", arguments, recipe.Environment)
	}
}

func TestNestedConfiguredArchiveRoleRunsInTypedWorkingTree(t *testing.T) {
	const target = "drivers/example/built-in.a"
	profile := mustCompactKbuildProfileForTest(t, "build:drivers/example", "scripts/Makefile.build", "drivers/example", `
cmd_ar = xargs $(AR) -r -c -T $@ < $<
`, map[string]string{"AR": KbuildActionRoleToken("target", "ar")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "drivers/example/members.list")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "drivers/example",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	sourceID, err := metadata.ensureActionPlanSource(plan, "drivers/example/members.list")
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "built-in", command: "ar",
	}, []compactKbuildRuleInput{{path: "drivers/example/members.list", sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	archive, ok := compactKbuildPlanNode(plan, producer)
	if !ok || archive.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("nested archive=%#v, want typed compound", archive)
	}
	recipe := plan.Recipes[archive.Recipe]
	if recipe.ExecutionDirectory != "drivers/example" || !slices.Contains(recipe.AuxiliaryTools, "ar") {
		t.Fatalf("nested archive recipe=%#v", recipe)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if !strings.Contains(script, "xargs ar -r -c -T ../../"+target) || strings.Contains(script, "${input:") {
		t.Fatalf("nested archive script=%q", script)
	}
}

func TestGenericKbuildArchivePreservesObjPatsubstAutomaticVariableSemantics(t *testing.T) {
	const (
		directory = "arch/x86/virt/svm"
		target    = directory + "/built-in.a"
	)
	prerequisites := []string{directory + "/cmdline.o", directory + "/nested.o"}
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
real-prereqs = $(filter-out FORCE,$^)
cmd_ar_builtin = rm -f $@; $(if $(real-prereqs), printf "$(obj)/%s " $(patsubst $(obj)/%,%,$(real-prereqs)) | xargs) $(AR) cDPrST $@
$(obj)/built-in.a: $(obj)/cmdline.o $(obj)/nested.o FORCE
	$(call if_changed,ar_builtin)
`, map[string]string{
		"AR":  KbuildActionRoleToken("target", "ar"),
		"obj": directory,
	})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	seedRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{"-c", "-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	inputs := make([]compactKbuildRuleInput, 0, len(prerequisites))
	for _, prerequisite := range prerequisites {
		seed := ActionPlanNode{
			Stage: "target", Kind: "compile", Tool: "cc", Product: "vmlinux",
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: prerequisite}},
		}
		prerequisiteProducer, err := appendActionPlanNode(plan, seed, seedRecipe)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, compactKbuildRuleInput{path: prerequisite, producer: prerequisiteProducer})
	}
	match, matched, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatalf("target %q did not select its source rule", target)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		withInitialObjectTree(true).
		forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, match, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 3; got != want {
		t.Fatalf("node count=%d, want two prerequisites + one atomic archive pipeline: %#v", got, plan.Nodes)
	}
	archive, ok := compactKbuildPlanNode(plan, producer)
	if !ok || archive.Tool != compactKbuildScriptRunnerRole || archive.Outputs[0].Path != target {
		t.Fatalf("atomic archive pipeline=%#v", archive)
	}
	recipe := plan.Recipes[archive.Recipe]
	if recipe.ExecutionDirectory != directory || recipe.Stdin != "" || recipe.Stdout != "" {
		t.Fatalf("archive pipeline cwd/streams=%#v", recipe)
	}
	if recipe.WorkingOutputs["00000000"] != target || len(recipe.WorkingInputs) != 2 {
		t.Fatalf("archive pipeline working tree=%#v", recipe)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	want := `printf "../../../../` + directory + `/%s " cmdline.o nested.o | xargs ar cDPrST ../../../../` + target
	if !strings.Contains(script, want) {
		t.Fatalf("archive pipeline script=%q, want source-derived obj prefix and basename data %q", script, want)
	}
	if strings.Contains(script, "${input:") || strings.Contains(script, ".linux-bzl-intermediate") {
		t.Fatalf("archive pipeline rewrote data or serialized its pipe: %q", script)
	}
}

func TestAtomicKbuildArchiveStagesThinArchiveMemberClosure(t *testing.T) {
	const (
		leafPath    = "init/main.o"
		archivePath = "built-in.a"
		target      = "vmlinux.a"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
real-prereqs = $(filter-out FORCE,$^)
cmd_ar_builtin = printf "%s " $(real-prereqs) | xargs $(AR) cDPrST $@
vmlinux.a: built-in.a FORCE
	$(call if_changed,ar_builtin)
`, map[string]string{"AR": KbuildActionRoleToken("target", "ar")})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	seedRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{"-c", "-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	leafProducer, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "compile", Tool: "cc", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: leafPath}},
	}, seedRecipe)
	if err != nil {
		t.Fatal(err)
	}
	archiveProducer, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "archive", Tool: "ar", Product: "vmlinux",
		Inputs:  []ActionPlanNodeEdge{{Role: "member", ProducerID: leafProducer}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: archivePath}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "archive", Tool: "ar",
		Arguments: []string{"cDPrST", "${output:00000000}"},
		Inputs:    []string{"member:00000000"}, Outputs: []string{"00000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	match, matched, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatalf("target %q did not select its source rule", target)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, match, []compactKbuildRuleInput{{
		path: archivePath, producer: archiveProducer,
	}})
	if err != nil {
		t.Fatal(err)
	}
	final, ok := compactKbuildPlanNode(plan, producer)
	if !ok || final.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("final atomic archive=%#v", final)
	}
	rolesByProducer := map[string]string{}
	for _, input := range final.Inputs {
		rolesByProducer[input.ProducerID] = input.Role
	}
	if got := rolesByProducer[archiveProducer]; got != "prerequisite" {
		t.Fatalf("direct thin archive role=%q, want prerequisite; inputs=%#v", got, final.Inputs)
	}
	if got, want := len(actionPlanNodeInputSetEntriesForTest(t, plan, final)), 1; got != want {
		t.Fatalf("atomic archive persistent inputs=%#v, want thin member only", actionPlanNodeInputSetEntriesForTest(t, plan, final))
	}
	memberInput, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, final, leafPath)
	wantMemberInput := ActionPlanInputSetEntry{
		Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: leafPath},
		ProducerID: leafProducer,
	}
	if !found || memberInput != wantMemberInput {
		t.Fatalf("atomic archive thin-member input = (%#v, %t), want %#v", memberInput, found, wantMemberInput)
	}
}

func TestKbuildThinArchiveLinkConsumerStagesMemberClosure(t *testing.T) {
	const (
		memberPath       = "init/main.o"
		sourceMemberPath = "firmware/prebuilt.o"
		sourceMemberID   = "src-00000001"
		unrelatedPath    = "drivers/unrelated.o"
		archivePath      = "vmlinux.a"
		copiedArchive    = "renamed-vmlinux.a"
		target           = "vmlinux.o"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:root", "scripts/Makefile.build", "", `
cmd_ar = rm -f $@; $(AR) cDPrsT $@ $(real-prereqs)
cmd_copy = cp $< $@
cmd_index = $(AR) s $@
cmd_ld = $(LD) -r -o $@ --whole-archive $< --no-whole-archive
`, map[string]string{
		"AR":           KbuildActionRoleToken("target", "ar"),
		"LD":           KbuildActionRoleToken("target", "ld"),
		"real-prereqs": memberPath + " " + sourceMemberPath,
	})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Recipes: map[string]ActionRecipe{},
		Sources: []ActionPlanSource{{ID: sourceMemberID, Namespace: "kernel", Path: sourceMemberPath}},
	}
	seedRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{"-c", "-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	seed := func(output string) string {
		producer, err := appendActionPlanNode(plan, ActionPlanNode{
			Stage: "target", Kind: "compile", Tool: "cc", Product: "vmlinux",
			Outputs: []ActionPlanOutput{{Tree: "objects", Path: output}},
		}, seedRecipe)
		if err != nil {
			t.Fatal(err)
		}
		return producer
	}
	memberProducer := seed(memberPath)
	unrelatedProducer := seed(unrelatedPath)
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	archiveProducer, err := builder.buildCommandTemplate(archivePath, compactKbuildRuleMatch{
		profile: profile, stem: "vmlinux", command: "ar",
	}, []compactKbuildRuleInput{
		{path: memberPath, producer: memberProducer},
		{path: sourceMemberPath, sourceID: sourceMemberID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.isPathSensitiveArchiveOutput(archiveProducer, 0) {
		t.Fatalf("thin archive output %s was not marked path-sensitive", archiveProducer)
	}
	copyProducer, err := builder.buildCommandTemplate(copiedArchive, compactKbuildRuleMatch{
		profile: profile, stem: "renamed-vmlinux", command: "copy",
	}, []compactKbuildRuleInput{{path: archivePath, producer: archiveProducer}})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.isPathSensitiveArchiveOutput(copyProducer, 0) {
		t.Fatalf("byte-for-byte archive projection %s did not preserve path sensitivity", copyProducer)
	}
	indexProducer, err := builder.buildCommandTemplate(copiedArchive, compactKbuildRuleMatch{
		profile: profile, stem: "renamed-vmlinux", command: "index",
	}, []compactKbuildRuleInput{{path: copiedArchive, producer: copyProducer}})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.isPathSensitiveArchiveOutput(indexProducer, 0) {
		t.Fatalf("in-place archive index %s did not preserve path sensitivity", indexProducer)
	}
	linkProducer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "vmlinux", command: "ld",
	}, []compactKbuildRuleInput{{path: copiedArchive, producer: indexProducer}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.isPathSensitiveArchiveOutput(linkProducer, 0) {
		t.Fatalf("relocatable link output %s inherited archive path sensitivity", linkProducer)
	}
	replacementProducer, err := builder.buildCommandTemplate(copiedArchive, compactKbuildRuleMatch{
		profile: profile, stem: "renamed-vmlinux", command: "copy",
	}, []compactKbuildRuleInput{
		{path: unrelatedPath, producer: unrelatedProducer},
		{path: copiedArchive, producer: indexProducer},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.isPathSensitiveArchiveOutput(replacementProducer, 0) {
		t.Fatalf("regular byte projection %s inherited the replaced destination's archive sensitivity", replacementProducer)
	}
	link, ok := compactKbuildPlanNode(plan, linkProducer)
	if !ok || link.Tool != "ld" || link.Outputs[0].Path != target {
		t.Fatalf("link=%#v", link)
	}
	rolesByProducer := map[string]string{}
	for _, input := range link.Inputs {
		rolesByProducer[input.ProducerID] = input.Role
	}
	if got := rolesByProducer[indexProducer]; got != "object" {
		t.Fatalf("direct archive role=%q, want object; inputs=%#v", got, link.Inputs)
	}
	if got := rolesByProducer[unrelatedProducer]; got != "" {
		t.Fatalf("unrelated object unexpectedly staged with role %q; inputs=%#v", got, link.Inputs)
	}
	if got, want := len(actionPlanNodeInputSetEntriesForTest(t, plan, link)), 3; got != want {
		t.Fatalf("thin archive link persistent inputs=%#v, want archive lineage plus generated and source members", actionPlanNodeInputSetEntriesForTest(t, plan, link))
	}
	for pathname, want := range map[string]ActionPlanInputSetEntry{
		archivePath: {
			Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: archivePath},
			ProducerID: archiveProducer,
		},
		memberPath: {
			Target:     ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: memberPath},
			ProducerID: memberProducer,
		},
		sourceMemberPath: {
			Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: sourceMemberPath},
			SourceID: sourceMemberID,
		},
	} {
		got, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, link, pathname)
		if !found || got != want {
			t.Fatalf("thin archive link persistent input for %q = (%#v, %t), want %#v", pathname, got, found, want)
		}
	}
	if unrelatedInput, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, link, unrelatedPath); found {
		t.Fatalf("unrelated object unexpectedly staged as persistent input %#v", unrelatedInput)
	}
	linkRecipe := plan.Recipes[link.Recipe]
	foundDirectArchive := false
	for binding, pathname := range linkRecipe.WorkingInputs {
		if pathname == unrelatedPath {
			t.Fatalf("unrelated object appears in link working inputs: %#v", linkRecipe.WorkingInputs)
		}
		if pathname == copiedArchive {
			foundDirectArchive = strings.HasPrefix(binding, "input:object:")
		}
	}
	if !foundDirectArchive {
		t.Fatalf("direct thin archive %q is absent from link working inputs: %#v", copiedArchive, linkRecipe.WorkingInputs)
	}
}

func TestKbuildThinArchiveClosureExcludesIncidentalConfigSource(t *testing.T) {
	const (
		configPath       = "include/config/kernel.release"
		archivePath      = "arch/x86/built-in.a"
		sourceMemberPath = "arch/x86/prebuilt.o"
		consumer         = "vmlinux.o"
	)
	configSourceID := "src-config"
	memberSourceID := "src-member"
	plan := &ActionPlan{
		Sources: []ActionPlanSource{
			{ID: configSourceID, Namespace: "config", Path: "kernel.release"},
			{ID: memberSourceID, Namespace: "kernel", Path: sourceMemberPath},
		},
		Recipes: map[string]ActionRecipe{},
	}
	configProducer, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "prep", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: configPath}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}"}, Outputs: []string{"00000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	archiveProducer, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "archive", Tool: "ar", Product: "vmlinux",
		Sources: []ActionPlanSourceEdge{
			{Role: "object", SourceID: memberSourceID},
			{Role: compactKbuildWorkingClosureInputRole, SourceID: configSourceID},
		},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: archivePath}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "archive", Tool: "ar",
		WorkingDirectory: "archive",
		Sources:          []string{"object:00000000", compactKbuildWorkingClosureInputRole + ":00000001"},
		Outputs:          []string{"00000000"},
		WorkingInputs: map[string]string{
			"source:object:00000000": sourceMemberPath,
			"source:" + compactKbuildWorkingClosureInputRole + ":00000001": configPath,
		},
		WorkingOutputs: map[string]string{"00000000": archivePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.markPathSensitiveArchiveOutput(archiveProducer, 0); err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	inputs, pathSensitive, err := builder.compactKbuildPathSensitiveArchiveClosureInputs(
		consumer,
		CompactKbuildProfile{Name: "build:root", Path: "Makefile"},
		[]compactKbuildRuleInput{{path: archivePath, producer: archiveProducer}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !pathSensitive {
		t.Fatal("thin archive closure was not selected")
	}
	byPath := map[string]compactKbuildRuleInput{}
	for _, input := range inputs {
		byPath[input.path] = input
	}
	if got := byPath[configPath]; got.producer != configProducer || got.sourceID != "" {
		t.Fatalf("config state = %#v, want exact generated producer %q", got, configProducer)
	}
	if got := byPath[sourceMemberPath]; got.sourceID != memberSourceID || got.producer != "" || !got.workingOnly {
		t.Fatalf("archive source member = %#v, want retained immutable member %q", got, memberSourceID)
	}
}

func TestKbuildThinArchiveClosureExpandsWorkingOnlyExactFrontier(t *testing.T) {
	const (
		memberPath  = "init/main.o"
		archivePath = "built-in.a"
		consumer    = "vmlinux.o"
	)
	member := ActionPlanNode{
		ID: strings.Repeat("a", 64), Stage: "target", Kind: "compile",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: memberPath}},
	}
	archive := ActionPlanNode{
		ID: strings.Repeat("b", 64), Stage: "target", Kind: "archive",
		Recipe: "archive-recipe",
		Inputs: []ActionPlanNodeEdge{{
			Role: "object", ProducerID: member.ID,
		}},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: archivePath}},
	}
	plan := &ActionPlan{
		Nodes:   []ActionPlanNode{member, archive},
		Recipes: map[string]ActionRecipe{"archive-recipe": {}},
	}
	if err := plan.markPathSensitiveArchiveOutput(archive.ID, 0); err != nil {
		t.Fatal(err)
	}
	artifact := CompactKbuildVisibleArtifact{
		Path: archivePath, Profile: "archive-owner", Target: archivePath,
	}
	profile := CompactKbuildProfile{Name: "consumer"}
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []CompactKbuildVisibleArtifact{artifact})
	builder := withMaterializedInitialObjectTreeArtifactForTest(
		t,
		newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).
			forOutput("target", "objects", "vmlinux"),
		artifact,
		"target", "target", "target",
		archive.ID,
	)
	baseline, err := builder.compactKbuildWorkingTreeClosureInputs(consumer, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline) != 1 || baseline[0].producer != archive.ID || !baseline[0].workingOnly {
		t.Fatalf("exact archive frontier = %#v, want only working-tree archive %q", baseline, archive.ID)
	}
	expanded, pathSensitive, err := builder.compactKbuildPathSensitiveArchiveClosureInputs(
		consumer,
		profile,
		baseline,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !pathSensitive {
		t.Fatal("working-tree-only thin archive closure was not selected")
	}
	byPath := map[string]compactKbuildRuleInput{}
	for _, input := range expanded {
		byPath[input.path] = input
	}
	if got := byPath[archivePath]; got.producer != archive.ID || !got.workingOnly {
		t.Fatalf("exact archive = %#v, want retained working-tree producer %q", got, archive.ID)
	}
	if got := byPath[memberPath]; got.producer != member.ID || !got.workingOnly {
		t.Fatalf("thin archive member = %#v, want working-tree member %q", got, member.ID)
	}
}

func TestAtomicKbuildThinArchiveIsIndependentOfPrivateWorkingRoot(t *testing.T) {
	for _, tool := range []string{"ar", "xargs", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required for thin-archive integration coverage", tool)
		}
	}
	const directory = "drivers/example"
	script := "#!/bin/sh\nset -e\nrm -f ../../" + directory + "/built-in.a; " +
		`printf "../../` + directory + `/%s " first.o second.o | xargs ar cDPrST ../../` + directory + "/built-in.a\n"
	archives := make([][]byte, 0, 2)
	for iteration := 0; iteration < 2; iteration++ {
		privateRoot := filepath.Join(t.TempDir(), fmt.Sprintf("private-%d", iteration))
		invocation := filepath.Join(privateRoot, filepath.FromSlash(directory))
		if err := os.MkdirAll(invocation, 0o755); err != nil {
			t.Fatal(err)
		}
		for index, name := range []string{"first.o", "second.o"} {
			if err := os.WriteFile(filepath.Join(invocation, name), []byte(fmt.Sprintf("object-%d", index)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		command := exec.Command("sh", "-c", script)
		command.Dir = invocation
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("execute thin-archive pipeline: %v\n%s", err, output)
		}
		archive, err := os.ReadFile(filepath.Join(invocation, "built-in.a"))
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{privateRoot, filepath.Base(privateRoot)} {
			if bytes.Contains(archive, []byte(private)) {
				t.Fatalf("thin archive contains private root %q: %q", private, archive)
			}
		}
		for _, member := range []string{"first.o/", "second.o/"} {
			if !bytes.Contains(archive, []byte(member)) {
				t.Fatalf("thin archive does not contain stable member %q: %q", member, archive)
			}
		}
		archives = append(archives, archive)
	}
	if !bytes.Equal(archives[0], archives[1]) {
		t.Fatalf("thin archive bytes depend on the private working root")
	}
}

func TestGenericKbuildRecipePublishesGroupedRuleOutputs(t *testing.T) {
	metadata, target := compactGenericRecipeMetadataForTest(
		t, `tools/filter $< generated/primary.h generated/sidecar.h`, "&:",
		"generated/primary.h", "generated/sidecar.h",
	)
	plan := compactGenericRecipePlanForTest()
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	result := plan.Nodes[1]
	if got, want := len(result.Outputs), 2; got != want {
		t.Fatalf("outputs=%#v, want %d grouped outputs", result.Outputs, want)
	}
	if result.Outputs[0].Path != "generated/primary.h" || result.Outputs[1].Path != "generated/sidecar.h" {
		t.Fatalf("outputs=%#v", result.Outputs)
	}
	recipe := plan.Recipes[result.Recipe]
	if recipe.WorkingOutputs["00000000"] != result.Outputs[0].Path || recipe.WorkingOutputs["00000001"] != result.Outputs[1].Path {
		t.Fatalf("working outputs=%#v", recipe.WorkingOutputs)
	}
}

func TestGenericKbuildRecipePublishesImplicitPatternPeerOutputs(t *testing.T) {
	const target = "scripts/dtc/dtc-parser.tab.h"
	profile := mustCompactKbuildProfileForTest(t, "build:scripts/dtc", "scripts/Makefile.host", "scripts/dtc", `
cmd_bison = $(YACC) -o $(basename $@).c --defines=$(basename $@).h -t -l $<
scripts/dtc/%.tab.c scripts/dtc/%.tab.h: scripts/dtc/%.y FORCE
	$(call if_changed,bison)
`, map[string]string{"YACC": KbuildActionRoleToken("host", "bison")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "scripts/dtc/dtc-parser.y")
	metadata := &CompactMetadata{
		actionRoles: testHostActionRoles("bison"),
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, metadata: metadata,
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("host", "host", "sdk")
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok || node.Stage != "host" || len(node.Outputs) != 2 {
		t.Fatalf("implicit multi-target pattern node = %#v, want two host outputs", node)
	}
	wantPaths := []string{"scripts/dtc/dtc-parser.tab.h", "scripts/dtc/dtc-parser.tab.c"}
	for index, want := range wantPaths {
		if node.Outputs[index].Tree != "host" || node.Outputs[index].Path != want {
			t.Fatalf("implicit multi-target outputs = %#v, want %q in host tree", node.Outputs, wantPaths)
		}
	}
	recipe := plan.Recipes[node.Recipe]
	if recipe.Tool != "bison" ||
		recipe.WorkingOutputs["00000000"] != "scripts/dtc/dtc-parser.tab.h" ||
		recipe.WorkingOutputs["00000001"] != "scripts/dtc/dtc-parser.tab.c" {
		t.Fatalf("implicit multi-target recipe = %#v", recipe)
	}
	arguments := strings.Join(recipe.Arguments, " ")
	for _, want := range []string{"-o scripts/dtc/dtc-parser.tab.c", "--defines=scripts/dtc/dtc-parser.tab.h"} {
		if !strings.Contains(arguments, want) {
			t.Errorf("bison arguments %q omit %q", arguments, want)
		}
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("implicit multi-target pattern plan validation: %v", err)
	}
}

func TestGenericKbuildRecipeKeepsExplicitColonTargetsIndependent(t *testing.T) {
	rule := KbuildRule{Targets: []string{"first.out", "second.out"}, Separator: ":"}
	if compactKbuildRuleHasGroupedOutputs(rule) {
		t.Fatal("ordinary explicit colon rule was classified as grouped")
	}
	match := compactKbuildRuleMatch{rule: rule}
	if got, want := compactKbuildRecipeRuleOutputs("first.out", match), []string{"first.out"}; !slices.Equal(got, want) {
		t.Fatalf("explicit colon outputs = %q, want %q", got, want)
	}
}

func TestGenericKbuildRecipeRejectsConditionalShellBranches(t *testing.T) {
	_, err := parseCompactKbuildRecipe(`tools/filter input || tools/fallback input`, compactKbuildAutomaticContext{target: "out"})
	if err == nil || !strings.Contains(err.Error(), `unsupported shell operator "||"`) {
		t.Fatalf("parse error=%v, want unsupported conditional branch", err)
	}
}

func TestGenericKbuildRecipeAcceptsTerminalSemicolon(t *testing.T) {
	commands, err := parseCompactKbuildRecipe(`printf '%s\n' ready;`, compactKbuildAutomaticContext{target: "out"})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].program != "printf" || commands[0].connector != ";" {
		t.Fatalf("commands = %#v, want one semicolon-terminated printf", commands)
	}
}

func TestGenericKbuildRecipePreservesPlanTreePlaceholders(t *testing.T) {
	commands, err := parseCompactKbuildRecipe(
		`${tree:prep}/tools/filter ${tree:kernel}/input.txt > $@`,
		compactKbuildAutomaticContext{target: "generated/result.h"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(commands), 1; got != want {
		t.Fatalf("command count=%d, want %d: %#v", got, want, commands)
	}
	if got, want := commands[0].program, `${tree:prep}/tools/filter`; got != want {
		t.Fatalf("program=%q, want %q", got, want)
	}
	if got, want := commands[0].arguments, []string{`${tree:kernel}/input.txt`}; !slices.Equal(got, want) {
		t.Fatalf("arguments=%q, want %q", got, want)
	}
}

func TestGenericKbuildRecipeCanonicalizesSourceRootExecutablePath(t *testing.T) {
	rooted := "/rbe/execroot/external/kernel/__LINUX_BZL_SOURCE_TREE__/scripts/mkcompile_h"
	if got, ok := compactKbuildCommandPath(rooted); !ok || got != "scripts/mkcompile_h" {
		t.Fatalf("compactKbuildCommandPath(%q)=(%q,%t), want source-relative generated tool", rooted, got, ok)
	}
	for _, marker := range []string{
		compactKbuildActionObjectTreeMarker,
		compactKbuildActionAbsoluteObjectTreeMarker,
	} {
		rootedOutput := "arch/x86/boot/compressed/" + marker + "/vmlinux.relocs"
		if got, ok := compactKbuildCommandPath(rootedOutput); !ok || got != "arch/x86/boot/compressed/vmlinux.relocs" {
			t.Fatalf("compactKbuildCommandPath(%q)=(%q,%t), want literal prefix around private object root", rootedOutput, got, ok)
		}
		if got := compactKbuildFinalizeRootedActionRecipeText(rootedOutput); got != "arch/x86/boot/compressed/vmlinux.relocs" {
			t.Fatalf("compactKbuildFinalizeRootedActionRecipeText(%q)=%q, want literal prefix around private object root", rootedOutput, got)
		}
	}
	embeddedSource := "generated/" + compactKbuildActionSourceTreeMarker + "/source.h"
	if got := compactKbuildCollapseEmbeddedActionObjectRoots(embeddedSource); got != embeddedSource {
		t.Fatalf("compactKbuildCollapseEmbeddedActionObjectRoots(%q)=%q, want source root unchanged", embeddedSource, got)
	}
	if got, want := compactKbuildFinalizeRootedActionRecipeText(embeddedSource), "generated/${tree:kernel}/source.h"; got != want {
		t.Fatalf("compactKbuildFinalizeRootedActionRecipeText(%q)=%q, want uncollapsed source root %q", embeddedSource, got, want)
	}
	publicObjectRoot := "/rbe/execroot/external/kernel/__LINUX_BZL_OBJECT_TREE__/vmlinux.relocs"
	if got, ok := compactKbuildCommandPath(publicObjectRoot); !ok || got != "vmlinux.relocs" {
		t.Fatalf("compactKbuildCommandPath(%q)=(%q,%t), want physical prefix before public object root discarded", publicObjectRoot, got, ok)
	}
	for command, want := range map[string]struct {
		path   string
		source bool
	}{
		"${tree:kernel}/scripts/mkcompile_h":                     {path: "scripts/mkcompile_h", source: true},
		"${tree:prep}/scripts/generated":                         {path: "scripts/generated"},
		"${tree:host}/scripts/generated":                         {path: "scripts/generated"},
		compactKbuildActionSourceTreeMarker + "/scripts/source":  {path: "scripts/source", source: true},
		compactKbuildActionObjectTreeMarker + "/scripts/dtc/dtc": {path: "scripts/dtc/dtc"},
	} {
		profile := CompactKbuildProfile{}
		got, source, ok := compactKbuildProfileCommandPath(profile, command)
		if !ok || got != want.path || source != want.source {
			t.Fatalf("compactKbuildProfileCommandPath(%q)=(%q,%t,%t), want (%q,%t,true)", command, got, source, ok, want.path, want.source)
		}
	}
}

func TestGenericKbuildRecipeRunsAbsoluteExtensionlessShellSourceHermetically(t *testing.T) {
	root := t.TempDir()
	mustWriteSource(t, root, "scripts/mkcompile_h", "#!/bin/sh\n")
	makefile := strings.ReplaceAll(`
MKCOMPILE_H = __SOURCE_ROOT__/scripts/mkcompile_h
cmd_compile_h = $(MKCOMPILE_H) $@
include/generated/compile.h: scripts/mkcompile_h FORCE
	$(call if_changed,compile_h)
`, "__SOURCE_ROOT__", filepath.ToSlash(root))
	kb, err := parseKbuildWithOptions(strings.NewReader(makefile), "Makefile", KbuildOptions{
		Variables: map[string]string{"srctree": root},
		SourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("prep", "Makefile", "", kb)
	if err != nil {
		t.Fatal(err)
	}
	target := "include/generated/compile.h"
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("node count=%d, want one hermetic source-script action: %#v", got, plan.Nodes)
	}
	consumer, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("consumer %q not found", producer)
	}
	recipe := plan.Recipes[consumer.Recipe]
	if consumer.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole ||
		len(recipe.ExecutableInputs) != 0 || !slices.ContainsFunc(consumer.Sources, func(input ActionPlanSourceEdge) bool { return input.Role == "script" }) {
		t.Fatalf("consumer=%#v recipe=%#v, want immutable extensionless source script %s", consumer, recipe, filepath.Join(root, "scripts/mkcompile_h"))
	}
}

func TestGenericKbuildRecipeRunsRelativeExtensionlessShellForFilechk(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "mkcompile_h"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	makefile := `
filechk_compile.h = scripts/mkcompile_h "arm64" "gcc version 14.2.0" "ld"
include/generated/compile.h: FORCE
	$(call filechk,compile.h)
`
	kb, err := parseKbuildWithOptions(strings.NewReader(makefile), "Makefile", KbuildOptions{
		Variables: map[string]string{"srctree": root},
		SourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
			"__LINUX_BZL_OBJECT_TREE__": root,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("prep", "Makefile", "", kb)
	if err != nil {
		t.Fatal(err)
	}
	target := "include/generated/compile.h"
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("node count=%d, want one hermetic source-script action: %#v", got, plan.Nodes)
	}
	consumer, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("consumer %q not found", producer)
	}
	recipe := plan.Recipes[consumer.Recipe]
	if consumer.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole ||
		len(recipe.ExecutableInputs) != 0 || !slices.ContainsFunc(consumer.Sources, func(input ActionPlanSourceEdge) bool { return input.Role == "script" }) {
		t.Fatalf("consumer=%#v recipe=%#v, want immutable relative extensionless source script", consumer, recipe)
	}
}

func TestGenericKbuildRecipeFoldsTemporaryMoveAndMutatesAliasedOutputInPlace(t *testing.T) {
	const (
		target  = "drivers/example/foo.o"
		source  = "drivers/example/foo.c"
		objtool = "tools/objtool/objtool"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:drivers/example", "scripts/Makefile.build", "drivers/example", `
cmd_objtool = ; $(objtool) --measured $@
cmd_ld_single = ; $(LD) -r -o $(obj)/.$(notdir $@).tmp $@; mv $(obj)/.$(notdir $@).tmp $@
cmd_cc_o_c = $(CC) -DCHAIN -c -o $@ $< $(cmd_ld_single) $(cmd_objtool)
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"), "LD": KbuildActionRoleToken("target", "ld"), "objtool": "__LINUX_BZL_OBJECT_TREE__/" + objtool,
		"obj": "drivers/example", "src": "drivers/example",
	})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "drivers/example",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{actionRoles: testConfiguredScopedActionRoles}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	seed := ActionPlanNode{
		Stage: "host", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "host", Path: objtool}},
	}
	seedRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
	}
	objtoolProducer, err := appendActionPlanNode(plan, seed, seedRecipe)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "foo", command: "cc_o_c",
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 4; got != want {
		t.Fatalf("node count=%d, want seed + compile + link + objtool: %#v", got, plan.Nodes)
	}
	compile, link, mutate := plan.Nodes[1], plan.Nodes[2], plan.Nodes[3]
	if compile.Tool != "cc" || compile.Outputs[0].Path != target ||
		!strings.HasPrefix(compile.Outputs[0].ArtifactPath, ".linux-bzl-intermediate/") ||
		actionPlanOutputIsCanonical(compile.Outputs[0]) {
		t.Fatalf("compile=%#v, want configured cc and private graph output", compile)
	}
	if link.Tool != "ld" || link.Outputs[0].Path != target ||
		!strings.HasPrefix(link.Outputs[0].ArtifactPath, ".linux-bzl-intermediate/") ||
		actionPlanOutputIsCanonical(link.Outputs[0]) {
		t.Fatalf("link=%#v, want configured ld and folded-move private graph output", link)
	}
	linkRecipe := plan.Recipes[link.Recipe]
	if !slices.Contains(linkRecipe.Arguments, "drivers/example/.foo.o.tmp") ||
		linkRecipe.WorkingOutputs["00000000"] != "drivers/example/.foo.o.tmp" {
		t.Fatalf("link recipe=%#v, want temporary move folded into declared output", linkRecipe)
	}
	mutateRecipe := plan.Recipes[mutate.Recipe]
	if producer != mutate.ID || mutate.Tool != "generated" || mutate.Outputs[0].Path != target ||
		mutateRecipe.WorkingOutputs["00000000"] != target || !slices.Contains(mutateRecipe.Arguments, "../../"+target) {
		t.Fatalf("in-place result=%#v recipe=%#v", mutate, mutateRecipe)
	}
	if len(mutateRecipe.ExecutableInputs) != 1 ||
		!slices.ContainsFunc(mutate.Inputs, func(input ActionPlanNodeEdge) bool { return input.ProducerID == objtoolProducer }) ||
		!slices.ContainsFunc(mutate.Inputs, func(input ActionPlanNodeEdge) bool { return input.ProducerID == link.ID }) {
		t.Fatalf("in-place inputs=%#v recipe=%#v", mutate.Inputs, mutateRecipe)
	}
}

func TestGenericKbuildRecipeKeepsTargetAndTrailingDepfileAtomic(t *testing.T) {
	const (
		target  = "drivers/of/empty_root.dtb"
		source  = "drivers/of/empty_root.dts"
		dtcPath = "scripts/dtc/dtc"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:drivers/of", "scripts/Makefile.dtbs", "drivers/of", `
DTC ?= $(objtree)/scripts/dtc/dtc
depfile = .$(notdir $@).d
dtc-tmp = .$(notdir $@).dts.tmp
cmd_dtc = $(HOSTCC) -E -Wp,-MMD,$(depfile).pre.tmp -o $(dtc-tmp) $<; $(DTC) -o $@ -d $(depfile).dtc.tmp $(dtc-tmp); cat $(depfile).pre.tmp $(depfile).dtc.tmp > $(depfile)
drivers/of/%.dtb: drivers/of/%.dts $(DTC) FORCE
	$(call if_changed_dep,dtc)
`, map[string]string{
		"HOSTCC":  KbuildActionRoleToken("host", "cc"),
		"objtree": "__LINUX_BZL_OBJECT_TREE__",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "drivers/of",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{actionRoles: testConfiguredScopedActionRoles}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	dtcNode := ActionPlanNode{
		Stage: "host", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "host", Path: dtcPath}},
	}
	dtcRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
	}
	dtcProducer, err := appendActionPlanNode(plan, dtcNode, dtcRecipe)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("host", "host", "sdk")
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "empty_root", command: "dtc",
	}, []compactKbuildRuleInput{
		{path: source, sourceID: sourceID},
		{path: dtcPath, producer: dtcProducer},
	})
	if err != nil {
		t.Fatal(err)
	}
	targetNode, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing target producer %q", producer)
	}
	targetRecipe := plan.Recipes[targetNode.Recipe]
	if targetNode.Tool != compactKbuildScriptRunnerRole || targetRecipe.Tool != compactKbuildScriptRunnerRole ||
		targetNode.Outputs[0].Path != target || targetRecipe.WorkingOutputs["00000000"] != target {
		t.Fatalf("target producer = %#v recipe = %#v, want one atomic compound publishing %q", targetNode, targetRecipe, target)
	}
	if got, want := len(plan.Nodes), 2; got != want {
		t.Fatalf("node count = %d, want generated DTC seed + atomic cmd_dtc: %#v", got, plan.Nodes)
	}
	if targetRecipe.ExecutionDirectory != "drivers/of" || len(targetRecipe.ExecutableInputs) != 1 ||
		!slices.ContainsFunc(targetNode.Inputs, func(input ActionPlanNodeEdge) bool { return input.ProducerID == dtcProducer }) {
		t.Fatalf("atomic cmd_dtc omitted typed cwd or generated executable: node=%#v recipe=%#v", targetNode, targetRecipe)
	}
	script := compactKbuildRecipeScriptContentForTest(t, targetRecipe)
	if strings.Contains(script, compactKbuildActionObjectTreeMarker) || strings.Contains(script, "__LINUX_BZL_OBJECT_TREE__") {
		t.Fatalf("atomic cmd_dtc leaked an object-tree analysis marker into its execution script: %q", script)
	}
	if !strings.Contains(script, "../../scripts/dtc/dtc") {
		t.Fatalf("atomic cmd_dtc script does not invoke its generated executable through the typed working tree: %q", script)
	}
	for _, want := range []string{"-Wp,-MMD,", ".pre.tmp", " -d ", ".dtc.tmp", "cat "} {
		if !strings.Contains(script, want) {
			t.Errorf("atomic cmd_dtc script omits %q: %q", want, script)
		}
	}
}

func TestGenericKbuildCmdAndFixdepKeepsAtomicCompilerMetadataBeforeOpaqueSuffix(t *testing.T) {
	const (
		target  = "drivers/example/foo.o"
		source  = "drivers/example/foo.c"
		fixdep  = "scripts/basic/fixdep"
		objtool = "tools/objtool/objtool"
		depfile = "drivers/example/.foo.o.d"
		cmdfile = "drivers/example/.foo.o.cmd"
		stack   = "drivers/example/foo.su"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:drivers/example", "scripts/Makefile.build", "drivers/example", `
dummy := y
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	sourceRoot := profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"]
	if err := os.WriteFile(filepath.Join(sourceRoot, source), []byte("#if defined(CONFIG_USED)\n#endif\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "drivers/example",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, metadata: metadata,
	}
	metadata.actionContracts = map[KbuildActionRoleRef]CompactKbuildActionContract{
		{Scope: "target", Role: "cc"}: {},
	}
	fixdepNode := ActionPlanNode{
		Stage: "host", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "host", Path: fixdep}},
	}
	fixdepRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
	}
	fixdepProducer, err := appendActionPlanNode(plan, fixdepNode, fixdepRecipe)
	if err != nil {
		t.Fatal(err)
	}
	objtoolNode := ActionPlanNode{
		Stage: "host", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "host", Path: objtool}},
	}
	objtoolProducer, err := appendActionPlanNode(plan, objtoolNode, fixdepRecipe)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	configSourceID, err := ensureActionPlanSource(plan, "config", "autoconf.h")
	if err != nil {
		t.Fatal(err)
	}
	ambientConfigSourceID, err := ensureActionPlanSource(plan, "config", "kernel.release")
	if err != nil {
		t.Fatal(err)
	}
	objectRoot := "__LINUX_BZL_OBJECT_TREE__/"
	temporary := "drivers/example/.tmp_foo.o"
	prefix := "set -e; echo 'CC foo.o'; "
	for _, signal := range []string{"HUP", "INT", "QUIT", "TERM", "PIPE"} {
		prefix += "trap 'rm -f " + objectRoot + target + "; trap - " + signal + "; kill -s " + signal + " $$' " + signal + "; "
	}
	compileAndFixdep := prefix + KbuildActionRoleToken("target", "cc") +
		" -Wp,-MMD," + objectRoot + depfile + " -include " + objectRoot + "include/generated/autoconf.h" +
		" -fstack-usage -c -o " + objectRoot + target + " __LINUX_BZL_SOURCE_TREE__/" + source +
		"; " + KbuildActionRoleToken("target", "ld") + " -r -o " + objectRoot + temporary + " " + objectRoot + target +
		"; mv " + objectRoot + temporary + " " + objectRoot + target +
		"; " + objectRoot + objtool + " --noinstr " + objectRoot + target +
		"; " + objectRoot + fixdep + " " + objectRoot + depfile + " " + objectRoot + target +
		" 'saved command' > " + objectRoot + cmdfile + "; rm -f " + objectRoot + depfile
	genObjtooldep := "{ echo; echo '" + target + ": $$(wildcard " + fixdep + ")'; } >> " + objectRoot + cmdfile
	match := compactKbuildRuleMatch{
		profile: profile,
		stem:    "foo",
		commandTemplates: []CompactKbuildCommandTemplate{
			{Name: "cc_o_c", Text: compileAndFixdep},
			{Name: "gen_objtooldep", Text: genObjtooldep},
		},
	}
	unsafeTemplate := "touch unsafe-prefix; " + strings.TrimPrefix(compileAndFixdep, prefix)
	unsafeRooted, err := compactKbuildRootedActionDirectRecipeText(profile, unsafeTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if _, split := compactKbuildCmdAndFixdepRecipeSplit(
		target,
		match,
		compactKbuildFinalizeRootedActionRecipeText(unsafeRooted),
		unsafeRooted,
		compactKbuildAutomaticContext{target: target, stem: "foo"},
	); split {
		t.Fatal("cmd_and_fixdep split accepted an opaque filesystem-producing prefix")
	}
	trapTemplate := "trap 'rm -f " + objectRoot + depfile + "' EXIT; " + strings.TrimPrefix(compileAndFixdep, prefix)
	trapRooted, err := compactKbuildRootedActionDirectRecipeText(profile, trapTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if _, split := compactKbuildCmdAndFixdepRecipeSplit(
		target,
		match,
		compactKbuildFinalizeRootedActionRecipeText(trapRooted),
		trapRooted,
		compactKbuildAutomaticContext{target: target, stem: "foo"},
	); split {
		t.Fatal("cmd_and_fixdep split accepted a trap-prefixed template")
	}
	arbitraryTrap := "trap 'echo unsafe' HUP; " + strings.TrimPrefix(compileAndFixdep, prefix)
	arbitraryRooted, err := compactKbuildRootedActionDirectRecipeText(profile, arbitraryTrap)
	if err != nil {
		t.Fatal(err)
	}
	if _, split := compactKbuildCmdAndFixdepRecipeSplit(
		target, match, compactKbuildFinalizeRootedActionRecipeText(arbitraryRooted), arbitraryRooted,
		compactKbuildAutomaticContext{target: target, stem: "foo"},
	); split {
		t.Fatal("cmd_and_fixdep split accepted an arbitrary signal trap")
	}
	foreignFixdep := strings.Replace(compileAndFixdep, objectRoot+fixdep+" ", objectRoot+"vendor/fixdep ", 1)
	foreignRooted, err := compactKbuildRootedActionDirectRecipeText(profile, foreignFixdep)
	if err != nil {
		t.Fatal(err)
	}
	if _, split := compactKbuildCmdAndFixdepRecipeSplit(
		target, match, compactKbuildFinalizeRootedActionRecipeText(foreignRooted), foreignRooted,
		compactKbuildAutomaticContext{target: target, stem: "foo"},
	); split {
		t.Fatal("cmd_and_fixdep split accepted a noncanonical generated fixdep")
	}
	sourceFixdep := strings.Replace(compileAndFixdep, objectRoot+fixdep+" ", "__LINUX_BZL_SOURCE_TREE__/"+fixdep+" ", 1)
	sourceFixdepRooted, err := compactKbuildRootedActionDirectRecipeText(profile, sourceFixdep)
	if err != nil {
		t.Fatal(err)
	}
	if _, split := compactKbuildCmdAndFixdepRecipeSplit(
		target, match, compactKbuildFinalizeRootedActionRecipeText(sourceFixdepRooted), sourceFixdepRooted,
		compactKbuildAutomaticContext{target: target, stem: "foo"},
	); split {
		t.Fatal("cmd_and_fixdep split accepted an immutable-source fixdep")
	}
	ldWithConfig := strings.Replace(
		compileAndFixdep,
		KbuildActionRoleToken("target", "ld")+" -r",
		KbuildActionRoleToken("target", "ld")+" -T "+objectRoot+"include/config/auto.conf -r",
		1,
	)
	ldWithConfigRooted, err := compactKbuildRootedActionDirectRecipeText(profile, ldWithConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, split := compactKbuildCmdAndFixdepRecipeSplit(
		target, match, compactKbuildFinalizeRootedActionRecipeText(ldWithConfigRooted), ldWithConfigRooted,
		compactKbuildAutomaticContext{target: target, stem: "foo"},
	); split {
		t.Fatal("cmd_and_fixdep split accepted a linker config-projection operand")
	}
	firstRooted, err := compactKbuildRootedActionDirectRecipeText(profile, compileAndFixdep)
	if err != nil {
		t.Fatal(err)
	}
	if _, split := compactKbuildCmdAndFixdepRecipeSplit(
		target, match, compactKbuildFinalizeRootedActionRecipeText(firstRooted), firstRooted,
		compactKbuildAutomaticContext{target: target, stem: "foo"},
	); !split {
		t.Fatal("exact compiler/ld/objtool/fixdep template was not recognized")
	}
	groupedMatch := match
	groupedMatch.rule = KbuildRule{
		Targets:   []string{target, "drivers/example/foo.peer"},
		Separator: "&:",
	}
	if _, split := compactKbuildCmdAndFixdepRecipeSplit(
		target, groupedMatch, compactKbuildFinalizeRootedActionRecipeText(firstRooted), firstRooted,
		compactKbuildAutomaticContext{target: target, stem: "foo"},
	); split {
		t.Fatal("cmd_and_fixdep split accepted a grouped-output rule")
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("target", "objects", "vmlinux").
		forProfile(profile)
	builder, err = builder.forObservedOutputs(target, []compactKbuildObservedOutput{{
		output: ActionPlanOutput{Tree: "metadata", Path: ".captures/foo.state"},
		path:   "drivers/example/opaque.side-effect",
	}})
	if err != nil {
		t.Fatal(err)
	}
	producer, err := builder.buildCommandTemplate(target, match, []compactKbuildRuleInput{
		{path: source, sourceID: sourceID},
		{path: fixdep, producer: fixdepProducer},
		{path: objtool, producer: objtoolProducer},
		{path: "include/generated/autoconf.h", sourceID: configSourceID, objectTree: true, workingOnly: true},
		{path: "include/config/kernel.release", sourceID: ambientConfigSourceID, objectTree: true, workingOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 4; got != want {
		t.Fatalf("node count = %d, want tool seeds + atomic compiler template + opaque suffix: %#v", got, plan.Nodes)
	}
	compile := plan.Nodes[2]
	compileRecipe := plan.Recipes[compile.Recipe]
	if compile.Kind != "compile" || compile.Tool != compactKbuildScriptRunnerRole ||
		compileRecipe.Tool != compactKbuildScriptRunnerRole || compileRecipe.CompilerInvocation == nil {
		t.Fatalf("compiler node = %#v recipe = %#v, want typed atomic scriptrun metadata", compile, compileRecipe)
	}
	if compileRecipe.CompilerInvocation.Tool != "cc" ||
		!slices.Contains(compileRecipe.CompilerInvocation.Arguments, "-include") ||
		!slices.ContainsFunc(compileRecipe.CompilerInvocation.Arguments, func(argument string) bool {
			return strings.Contains(argument, depfile)
		}) {
		t.Fatalf("compiler dependency metadata = %#v, want exact cc argv", compileRecipe.CompilerInvocation)
	}
	if !compileRecipe.CompilerInvocation.WorkingInputUsesComplete {
		t.Fatalf("compiler dependency working-input projection is incomplete: %#v", compileRecipe.CompilerInvocation)
	}
	usedWorkingPaths := map[string]bool{}
	for _, reference := range compileRecipe.CompilerInvocation.WorkingInputUses {
		usedWorkingPaths[compileRecipe.WorkingInputs[reference]] = true
	}
	for _, pathname := range []string{source, fixdep, objtool} {
		if !usedWorkingPaths[pathname] {
			t.Errorf("compiler dependency working-input projection omits %q: uses=%#v working=%#v", pathname, compileRecipe.CompilerInvocation.WorkingInputUses, compileRecipe.WorkingInputs)
		}
	}
	if got, want := len(actionPlanNodeInputSetEntriesForTest(t, plan, compile)), 2; got != want {
		t.Fatalf("compiler persistent inputs = %#v, want compiler config and ambient config", actionPlanNodeInputSetEntriesForTest(t, plan, compile))
	}
	for pathname, want := range map[string]ActionPlanInputSetEntry{
		"include/generated/autoconf.h": {
			Target:      ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "include/generated/autoconf.h"},
			SourceID:    configSourceID,
			CompilerUse: true,
		},
		"include/config/kernel.release": {
			Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "include/config/kernel.release"},
			SourceID: ambientConfigSourceID,
		},
	} {
		got, found := actionPlanNodeInputSetEntryForPathForTest(t, plan, compile, pathname)
		if !found || got != want {
			t.Errorf("compiler persistent input for %q = (%#v, %t), want %#v", pathname, got, found, want)
		}
	}
	if usedWorkingPaths["include/config/kernel.release"] {
		t.Errorf("compiler dependency working-input projection retained ambient kernel release: %#v", usedWorkingPaths)
	}
	auxiliaryWorkingPaths := map[string]bool{}
	for _, reference := range compileRecipe.CompilerInvocation.AuxiliaryWorkingInputUses {
		auxiliaryWorkingPaths[compileRecipe.WorkingInputs[reference]] = true
	}
	for _, pathname := range []string{fixdep, objtool} {
		if !auxiliaryWorkingPaths[pathname] {
			t.Errorf("auxiliary working-input projection omits %q: uses=%#v working=%#v", pathname, compileRecipe.CompilerInvocation.AuxiliaryWorkingInputUses, compileRecipe.WorkingInputs)
		}
	}
	for _, pathname := range []string{source, "include/generated/autoconf.h"} {
		if auxiliaryWorkingPaths[pathname] {
			t.Errorf("auxiliary projection incorrectly attributes compiler-only %q to a surrounding command", pathname)
		}
	}
	for _, pathname := range []string{target, cmdfile, stack} {
		outputIndex := slices.IndexFunc(compile.Outputs, func(output ActionPlanOutput) bool {
			return output.Path == pathname && output.ObservedPath == ""
		})
		if outputIndex < 0 || actionPlanOutputIsCanonical(compile.Outputs[outputIndex]) {
			t.Errorf("compiler outputs = %#v, want private atomic output %q", compile.Outputs, pathname)
		}
	}
	if slices.ContainsFunc(compile.Outputs, func(output ActionPlanOutput) bool { return output.Path == depfile }) ||
		!slices.ContainsFunc(compile.Outputs, func(output ActionPlanOutput) bool {
			return output.ObservedPath == "drivers/example/opaque.side-effect" && output.Path != ".captures/foo.state"
		}) {
		t.Fatalf("atomic compiler lost private observation or leaked depfile: %#v", compile.Outputs)
	}
	compileScript := compactKbuildRecipeScriptContentForTest(t, compileRecipe)
	for _, want := range []string{"set -e", "echo 'CC foo.o'", "trap ", " -c -o ", " -r -o ", "mv ", objtool, fixdep, "rm -f"} {
		if !strings.Contains(compileScript, want) {
			t.Errorf("atomic compiler script omits %q: %q", want, compileScript)
		}
	}
	if strings.Contains(compileScript, "wildcard") {
		t.Fatalf("atomic compiler script contains later selected template: %q", compileScript)
	}
	suffix, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing suffix producer %q", producer)
	}
	suffixRecipe := plan.Recipes[suffix.Recipe]
	if suffix.Kind != "generate" || suffix.Tool != compactKbuildScriptRunnerRole ||
		suffixRecipe.Tool != compactKbuildScriptRunnerRole || len(suffix.Inputs) < 2 {
		t.Fatalf("suffix = %#v recipe = %#v, want opaque scriptrun", suffix, suffixRecipe)
	}
	if !slices.ContainsFunc(suffix.Inputs, func(input ActionPlanNodeEdge) bool {
		return input.ProducerID == compile.ID && input.Role == "observed-state"
	}) {
		t.Errorf("suffix inputs = %#v, missing first-template observed-state frontier", suffix.Inputs)
	}
	for _, pathname := range []string{target, cmdfile, stack} {
		found := false
		for _, value := range suffixRecipe.WorkingInputs {
			found = found || value == pathname
		}
		if !found {
			t.Errorf("suffix working inputs = %#v, missing compiler output %q", suffixRecipe.WorkingInputs, pathname)
		}
	}
	for _, pathname := range []string{target, cmdfile} {
		if !slices.ContainsFunc(suffix.Outputs, func(output ActionPlanOutput) bool { return output.Path == pathname }) {
			t.Errorf("suffix outputs = %#v, missing %q", suffix.Outputs, pathname)
		}
	}
	if !slices.ContainsFunc(suffix.Outputs, func(output ActionPlanOutput) bool {
		return output.ObservedPath == "drivers/example/opaque.side-effect"
	}) {
		t.Errorf("opaque suffix did not retain final observed-state ownership: %#v", suffix.Outputs)
	}
	firstStack := slices.IndexFunc(compile.Outputs, func(output ActionPlanOutput) bool { return output.Path == stack })
	if firstStack < 0 {
		t.Fatalf("atomic compiler omitted persistent output %q: %#v", stack, compile.Outputs)
	}
	if !slices.ContainsFunc(suffix.Outputs, func(output ActionPlanOutput) bool {
		return output.Path == stack && output.persistent && output.ArtifactPath != compile.Outputs[firstStack].ArtifactPath
	}) {
		t.Errorf("opaque suffix did not republish persistent compiler output %q at a fresh artifact: first=%#v suffix=%#v", stack, compile.Outputs, suffix.Outputs)
	}
	script := compactKbuildRecipeScriptContentForTest(t, suffixRecipe)
	if strings.Contains(script, KbuildActionRoleToken("target", "cc")) || strings.Contains(script, " -c -o ") ||
		strings.Contains(script, "rm -f") {
		t.Fatalf("opaque suffix still contains the atomic first template: %q", script)
	}
	for _, want := range []string{"wildcard", cmdfile} {
		if !strings.Contains(script, want) {
			t.Errorf("opaque suffix omits %q: %q", want, script)
		}
	}
	selection := compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "target"}
	plan.selectionGraph = &compactKbuildSelectionGraph{
		profiles:                    map[string]CompactKbuildProfile{profile.Name: profile},
		selections:                  map[compactKbuildSelectionKey]CompactKbuildSelection{selection: {Profile: profile.Name}},
		materializedProducers:       map[compactKbuildSelectionKey]string{selection: compile.ID},
		selectionInitialArtifacts:   map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{},
		selectionGeneratedArtifacts: map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{},
	}
	compileDependencies, err := AnalyzeActionPlanNodeConfigDependencies(plan, compile)
	if err != nil {
		t.Fatal(err)
	}
	if compileDependencies.Opaque || !slices.Equal(compileDependencies.Symbols, []string{"CONFIG_USED"}) {
		t.Fatalf("compiler dependencies = %#v, want dynamically scanned CONFIG_USED", compileDependencies)
	}
	plan.metadata.configFragment = map[string]string{"CONFIG_MODVERSIONS": "y"}
	modversionsDependencies, err := AnalyzeActionPlanNodeConfigDependencies(plan, compile)
	if err != nil {
		t.Fatal(err)
	}
	if !modversionsDependencies.Opaque || !strings.Contains(modversionsDependencies.Reason, "MODVERSIONS") {
		t.Fatalf("MODVERSIONS compiler dependencies = %#v, want opaque", modversionsDependencies)
	}
	plan.metadata.configFragment = nil
	dependencies, err := AnalyzeActionPlanNodeConfigDependencies(plan, suffix)
	if err != nil {
		t.Fatal(err)
	}
	if !dependencies.Opaque {
		t.Fatalf("suffix dependencies = %#v, want full-config opaque classification", dependencies)
	}

	singleMetadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	singlePlan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, metadata: singleMetadata,
	}
	singleFixdep, err := appendActionPlanNode(singlePlan, fixdepNode, fixdepRecipe)
	if err != nil {
		t.Fatal(err)
	}
	singleObjtool, err := appendActionPlanNode(singlePlan, objtoolNode, fixdepRecipe)
	if err != nil {
		t.Fatal(err)
	}
	singleSource, err := singleMetadata.ensureActionPlanSource(singlePlan, source)
	if err != nil {
		t.Fatal(err)
	}
	singleConfig, err := ensureActionPlanSource(singlePlan, "config", "autoconf.h")
	if err != nil {
		t.Fatal(err)
	}
	singleMatch := match
	singleMatch.commandTemplates = slices.Clone(match.commandTemplates[:1])
	singleBuilder := newCompactKbuildRulePlanBuilder(singleMetadata, singlePlan).
		forOutput("target", "objects", "vmlinux").
		forProfile(profile)
	singleProducer, err := singleBuilder.buildCommandTemplate(target, singleMatch, []compactKbuildRuleInput{
		{path: source, sourceID: singleSource},
		{path: fixdep, producer: singleFixdep},
		{path: objtool, producer: singleObjtool},
		{path: "include/generated/autoconf.h", sourceID: singleConfig, objectTree: true, workingOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	singleNode, ok := compactKbuildPlanNode(singlePlan, singleProducer)
	if !ok || singleNode.Kind != "compile" || singleNode.Tool != compactKbuildScriptRunnerRole ||
		singlePlan.Recipes[singleNode.Recipe].CompilerInvocation == nil {
		t.Fatalf("single-template cmd_and_fixdep = node %#v recipe %#v, want final typed compound compile", singleNode, singlePlan.Recipes[singleNode.Recipe])
	}
	for _, pathname := range []string{target, cmdfile, stack} {
		if !slices.ContainsFunc(singleNode.Outputs, func(output ActionPlanOutput) bool {
			return output.Path == pathname && (pathname == stack || actionPlanOutputIsCanonical(output))
		}) {
			t.Errorf("single-template outputs = %#v, missing final %q", singleNode.Outputs, pathname)
		}
	}

	plan.Products = []ActionPlanProduct{{Name: "vmlinux", Tree: "objects", Path: target}}
	configDependencies := make(map[string]ConfigDependencySet, len(plan.Nodes))
	for _, node := range plan.Nodes {
		switch {
		case node.Kind == "compile":
			configDependencies[node.ID] = compileDependencies
		case node.Tool == compactKbuildScriptRunnerRole:
			configDependencies[node.ID] = dependencies
		default:
			configDependencies[node.ID] = ConfigDependencySet{}
		}
	}
	baseSnapshot, err := canonicalActionPlanSnapshot(plan, configDependencies, familyTestConfig("y", "n"))
	if err != nil {
		t.Fatal(err)
	}
	overlaySnapshot, err := canonicalActionPlanSnapshot(plan, configDependencies, familyTestConfig("y", "m"))
	if err != nil {
		t.Fatal(err)
	}
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: baseSnapshot},
		{Name: "irrelevant", Snapshot: overlaySnapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	sharedCompiles, localSuffixes := 0, 0
	for _, node := range family.Nodes {
		members := family.Memberships[node.ID]
		switch {
		case node.Kind == "compile":
			if slices.Equal(members, []string{"base", "irrelevant"}) {
				sharedCompiles++
			}
		case node.Tool == compactKbuildScriptRunnerRole && len(members) == 1:
			localSuffixes++
		}
	}
	if sharedCompiles != 1 || localSuffixes != 2 {
		t.Fatalf(
			"family split reuse = %d shared compiles, %d variant-local suffixes; want 1 and 2: memberships=%#v nodes=%#v",
			sharedCompiles, localSuffixes, family.Memberships, family.Nodes,
		)
	}
}

func TestGenericKbuildCmdAndFixdepRepublishesRequiredSideOutputFinalState(t *testing.T) {
	const (
		target  = "drivers/example/foo.o"
		source  = "drivers/example/foo.c"
		fixdep  = "scripts/basic/fixdep"
		depfile = "drivers/example/.foo.o.d"
		state   = "drivers/example/.foo.o.cmd"
		stack   = "drivers/example/foo.su"
	)
	for name, suffix := range map[string]string{
		"conditional append":                    "if true; then printf '%s\\n' '#SYMVER example 0x12345678' >> __LINUX_BZL_OBJECT_TREE__/" + state + "; fi",
		"inactive append":                       "if false; then printf '%s\\n' '#SYMVER example 0x12345678' >> __LINUX_BZL_OBJECT_TREE__/" + state + "; fi",
		"conditional overwrite":                 "if true; then printf '%s\\n' replacement > __LINUX_BZL_OBJECT_TREE__/" + state + "; fi",
		"conditional deletion requires failure": "if true; then rm -f __LINUX_BZL_OBJECT_TREE__/" + state + "; fi",
		"no suffix":                             "",
	} {
		t.Run(name, func(t *testing.T) {
			profile := mustCompactKbuildProfileForTest(t, "build:side-state", "scripts/Makefile.build", "drivers/example", "dummy := y\n", nil)
			profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: CompactKbuildInvocationObjectTree, Directory: "drivers/example",
			}); err != nil {
				t.Fatal(err)
			}
			metadata := &CompactMetadata{
				actionRoles: testConfiguredScopedActionRoles,
				Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
				actionContracts: map[KbuildActionRoleRef]CompactKbuildActionContract{
					{Scope: "target", Role: "cc"}: {},
				},
			}
			plan := &ActionPlan{
				Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
				Recipes:  map[string]ActionRecipe{}, metadata: metadata,
			}
			fixdepProducer, err := appendActionPlanNode(plan, ActionPlanNode{
				Stage: "host", Kind: "generate", Tool: "actionfile", Product: "sdk",
				Outputs: []ActionPlanOutput{{Tree: "host", Path: fixdep}},
			}, ActionRecipe{
				Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
				Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""},
				Outputs:   []string{"00000000"},
			})
			if err != nil {
				t.Fatal(err)
			}
			sourceID, err := metadata.ensureActionPlanSource(plan, source)
			if err != nil {
				t.Fatal(err)
			}
			objectRoot := "__LINUX_BZL_OBJECT_TREE__/"
			first := KbuildActionRoleToken("target", "cc") +
				" -Wp,-MMD," + objectRoot + depfile + " -fstack-usage -c -o " + objectRoot + target +
				" __LINUX_BZL_SOURCE_TREE__/" + source + "; " + objectRoot + fixdep + " " +
				objectRoot + depfile + " " + objectRoot + target + " 'saved command' > " +
				objectRoot + state + "; rm -f " + objectRoot + depfile
			match := compactKbuildRuleMatch{
				profile: profile, stem: "foo",
				commandTemplates: []CompactKbuildCommandTemplate{{Name: "first", Text: first}},
			}
			if suffix != "" {
				match.commandTemplates = append(match.commandTemplates, CompactKbuildCommandTemplate{Name: "suffix", Text: suffix})
			}
			builder := newCompactKbuildRulePlanBuilder(metadata, plan).
				forOutput("target", "objects", "vmlinux").forProfile(profile)
			producer, err := builder.buildCommandTemplate(target, match, []compactKbuildRuleInput{
				{path: source, sourceID: sourceID}, {path: fixdep, producer: fixdepProducer},
			})
			if err != nil {
				t.Fatal(err)
			}
			wantNodes := 3
			if suffix == "" {
				wantNodes = 2
			}
			if len(plan.Nodes) != wantNodes {
				t.Fatalf("nodes = %d, want %d", len(plan.Nodes), wantNodes)
			}
			firstNode := plan.Nodes[1]
			firstSlot := slices.IndexFunc(firstNode.Outputs, func(output ActionPlanOutput) bool {
				return output.Path == state && output.ObservedPath == ""
			})
			if firstNode.Kind != "compile" || firstSlot < 0 || firstNode.Outputs[firstSlot].persistent {
				t.Fatalf("first output lost typed compile or ordinary side-output identity: %#v", firstNode)
			}
			finalNode, found := compactKbuildPlanNode(plan, producer)
			if !found {
				t.Fatalf("missing final producer %s", producer)
			}
			finalSlot := slices.IndexFunc(finalNode.Outputs, func(output ActionPlanOutput) bool {
				return output.Path == state && output.ObservedPath == ""
			})
			if finalSlot < 0 {
				t.Fatalf("final producer does not publish required final state: %#v", finalNode.Outputs)
			}
			if finalNode.Outputs[finalSlot].persistent {
				t.Fatal("ordinary shell side output acquired persistent compiler/SDK provenance")
			}
			recipe := plan.Recipes[finalNode.Recipe]
			if recipe.WorkingOutputs[planOrdinal(finalSlot)] != state {
				t.Fatalf("final working outputs = %#v, want ordinary required %s", recipe.WorkingOutputs, state)
			}
			if suffix != "" {
				if finalNode.ID == firstNode.ID ||
					actionPlanOutputArtifactPath(finalNode.Outputs[finalSlot]) == actionPlanOutputArtifactPath(firstNode.Outputs[firstSlot]) {
					t.Fatal("suffix reused the pre-mutation producer or physical output identity")
				}
				if !slices.ContainsFunc(finalNode.Inputs, func(edge ActionPlanNodeEdge) bool {
					return edge.ProducerID == firstNode.ID && edge.Slot == firstSlot
				}) || !slices.Contains(sortedStringMapValues(recipe.WorkingInputs), state) {
					t.Fatal("suffix lost exact predecessor side-output bytes")
				}
				if finalNode.Kind != "generate" || strings.Contains(compactKbuildRecipeScriptContentForTest(t, recipe), " -c -o ") {
					t.Fatal("opaque suffix absorbed or repeated the first compiler command")
				}
				// Ordinary state is republished separately from compiler-produced
				// stack metadata; that metadata retains its existing SDK contract.
				if !slices.ContainsFunc(finalNode.Outputs, func(output ActionPlanOutput) bool {
					return output.Path == stack && output.persistent
				}) {
					t.Fatal("persistent compiler product was demoted while carrying ordinary state")
				}
			}
			if _, err := plan.entries(); err != nil {
				t.Fatalf("invalid final producer/slot ownership: %v", err)
			}
		})
	}
}

func TestGenericKbuildGroupedCmdAndFixdepFallsBackToOneOpaqueProducer(t *testing.T) {
	const (
		target  = "drivers/example/foo.o"
		peer    = "drivers/example/foo.peer"
		source  = "drivers/example/foo.c"
		fixdep  = "scripts/basic/fixdep"
		depfile = "drivers/example/.foo.o.d"
		cmdfile = "drivers/example/.foo.o.cmd"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:grouped-cmd-and-fixdep", "scripts/Makefile.build", "drivers/example", `
dummy := y
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "drivers/example",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	metadata.actionContracts = map[KbuildActionRoleRef]CompactKbuildActionContract{
		{Scope: "target", Role: "cc"}: {},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, metadata: metadata,
	}
	fixdepNode := ActionPlanNode{
		Stage: "host", Kind: "generate", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "host", Path: fixdep}},
	}
	fixdepRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
	}
	fixdepProducer, err := appendActionPlanNode(plan, fixdepNode, fixdepRecipe)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	objectRoot := "__LINUX_BZL_OBJECT_TREE__/"
	compileAndFixdep := KbuildActionRoleToken("target", "cc") +
		" -Wp,-MMD," + objectRoot + depfile + " -c -o " + objectRoot + target + " __LINUX_BZL_SOURCE_TREE__/" + source +
		"; " + objectRoot + fixdep + " " + objectRoot + depfile + " " + objectRoot + target +
		" 'saved command' > " + objectRoot + cmdfile + "; rm -f " + objectRoot + depfile
	publishPeer := "cp " + objectRoot + target + " " + objectRoot + peer
	match := compactKbuildRuleMatch{
		profile: profile,
		rule: KbuildRule{
			Targets:   []string{"drivers/example/%.o", "drivers/example/%.peer"},
			Separator: ":",
		},
		stem: "foo",
		commandTemplates: []CompactKbuildCommandTemplate{
			{Name: "cc_o_c", Text: compileAndFixdep},
			{Name: "publish_peer", Text: publishPeer},
		},
	}
	rootedFirst, err := compactKbuildRootedActionDirectRecipeText(profile, compileAndFixdep)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryMatch := match
	ordinaryMatch.rule = KbuildRule{Targets: []string{target}, Separator: ":"}
	if _, split := compactKbuildCmdAndFixdepRecipeSplit(
		target, ordinaryMatch, compactKbuildFinalizeRootedActionRecipeText(rootedFirst), rootedFirst,
		compactKbuildAutomaticContext{target: target, stem: "foo"},
	); !split {
		t.Fatal("fixture first template is not independently eligible for cmd_and_fixdep splitting")
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("target", "objects", "vmlinux").
		forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, match, []compactKbuildRuleInput{
		{path: source, sourceID: sourceID},
		{path: fixdep, producer: fixdepProducer},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 2; got != want {
		t.Fatalf("node count = %d, want fixdep seed + one unsplit grouped producer: %#v", got, plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing grouped producer %q", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Kind == "compile" || node.Tool != compactKbuildScriptRunnerRole ||
		recipe.Tool != compactKbuildScriptRunnerRole || recipe.CompilerInvocation != nil {
		t.Fatalf("grouped producer = %#v recipe = %#v, want one opaque unsplit scriptrun", node, recipe)
	}
	for _, pathname := range []string{target, peer} {
		if !slices.ContainsFunc(node.Outputs, func(output ActionPlanOutput) bool {
			return output.Tree == "objects" && output.Path == pathname
		}) {
			t.Errorf("grouped outputs = %#v, missing %q", node.Outputs, pathname)
		}
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	for _, want := range []string{" -c -o ", fixdep, "rm -f", "cp ", peer} {
		if !strings.Contains(script, want) {
			t.Errorf("unsplit grouped script omits %q: %q", want, script)
		}
	}
}

func TestHermeticKbuildCompoundExecutesEarlierExplicitOutput(t *testing.T) {
	const (
		target  = "scripts/basic/fixdep"
		source  = "scripts/basic/fixdep.c"
		depfile = "scripts/basic/.fixdep.d"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:scripts/basic", "scripts/Makefile.build", "", `
scripts/basic/fixdep: scripts/basic/fixdep.c FORCE
	@true
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
		metadata: metadata,
	}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	rootedTarget := compactKbuildActionObjectTreeMarker + "/" + target
	rootedDepfile := compactKbuildActionObjectTreeMarker + "/" + depfile
	template := KbuildActionRoleToken("host", "cc") +
		" -Wp,-MMD," + rootedDepfile + " -c -o " + rootedTarget + " " + source +
		"; " + rootedTarget + " " + rootedDepfile + " " + rootedTarget +
		" 'saved command' > " + compactKbuildActionObjectTreeMarker + "/scripts/basic/.fixdep.cmd; rm -f " + rootedDepfile
	commands, err := parseCompactKbuildRecipe(template, compactKbuildAutomaticContext{target: target})
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("prehost", "prehost", "sdk")
	producer, err := builder.buildHermeticKbuildCompound(target, compactKbuildRuleMatch{
		profile: profile,
		rule:    profile.Rules[0],
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}}, template, commands)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("node count=%d, want one atomic producer: %#v", got, plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing producer %q", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Stage != "prehost" || node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("producer=%#v recipe=%#v, want prehost scriptrun", node, recipe)
	}
	if len(recipe.ExecutableInputs) != 0 {
		t.Fatalf("locally produced executable became a graph input: %#v", recipe.ExecutableInputs)
	}
	hasSourceInput, hasSelfInput := false, false
	for _, inputPath := range recipe.WorkingInputs {
		hasSourceInput = hasSourceInput || inputPath == source
		hasSelfInput = hasSelfInput || inputPath == target
	}
	if !hasSourceInput || hasSelfInput {
		t.Fatalf("working inputs=%#v, want source but no self input", recipe.WorkingInputs)
	}
	hasTargetOutput, hasCommandOutput, hasDepfileOutput := false, false, false
	for _, outputPath := range recipe.WorkingOutputs {
		hasTargetOutput = hasTargetOutput || outputPath == target
		hasCommandOutput = hasCommandOutput || outputPath == "scripts/basic/.fixdep.cmd"
		hasDepfileOutput = hasDepfileOutput || outputPath == depfile
	}
	if !hasTargetOutput || !hasCommandOutput || hasDepfileOutput {
		t.Fatalf("working outputs=%#v, want target and surviving command state but no removed depfile", recipe.WorkingOutputs)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	compileOffset := strings.Index(script, " -c -o '"+target+"' '${tree:kernel}/"+source+"'")
	executeOffset := strings.Index(script, target+" "+depfile+" "+target)
	removeOffset := strings.Index(script, "rm -f "+depfile)
	if compileOffset < 0 || executeOffset <= compileOffset || removeOffset <= executeOffset {
		t.Fatalf("atomic producer/execute/cleanup order was not preserved: %q", script)
	}
	if err := plan.WriteStages(actionPlanStageOutputsForTest(filepath.Join(t.TempDir(), "plan"))); err != nil {
		t.Fatalf("write action plan: %v", err)
	}
}

func TestCompactKbuildCmdAndFixdepRecognizesCanonicalSelfBootstrap(t *testing.T) {
	const (
		target          = "scripts/basic/fixdep"
		source          = "scripts/basic/fixdep.c"
		depfile         = "scripts/basic/.fixdep.d"
		cmdfile         = "scripts/basic/.fixdep.cmd"
		xallocHeader    = "scripts/include/xalloc.h"
		nestedHeader    = "scripts/include/linux/nested.h"
		unrelatedSource = "scripts/not-included.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:scripts/basic", "scripts/Makefile.build", "scripts/basic", `
scripts/basic/fixdep: scripts/basic/fixdep.c FORCE
	@true
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	mustWriteSource(t, profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"], source, `
#include <xalloc.h>
static const char config_prefix[] = "CONFIG_";
int main(void) { return config_prefix[0] == 'C' ? 0 : 1; }
`)
	mustWriteSource(t, profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"], xallocHeader, "#include \"linux/nested.h\"\n#define xmalloc(size) malloc(size)\n")
	mustWriteSource(t, profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"], nestedHeader, "#define NESTED_HEADER 1\n")
	mustWriteSource(t, profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"], unrelatedSource, "int unrelated;\n")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "scripts/basic",
	}); err != nil {
		t.Fatal(err)
	}
	objectRoot := "__LINUX_BZL_OBJECT_TREE__/"
	template := KbuildActionRoleToken("host", "cc") +
		" -Wp,-MMD," + objectRoot + depfile + " -I__LINUX_BZL_SOURCE_TREE__/scripts/include -o " + objectRoot + target +
		" __LINUX_BZL_SOURCE_TREE__/" + source +
		"; " + objectRoot + target + " " + objectRoot + depfile + " " + objectRoot + target +
		" 'saved command' > " + objectRoot + cmdfile + "; rm -f " + objectRoot + depfile
	rooted, err := compactKbuildRootedActionDirectRecipeText(profile, template)
	if err != nil {
		t.Fatal(err)
	}
	match := compactKbuildRuleMatch{profile: profile}
	if _, split := compactKbuildCmdAndFixdepRecipeSplit(
		target, match, compactKbuildFinalizeRootedActionRecipeText(rooted), rooted,
		compactKbuildAutomaticContext{target: target},
	); !split {
		t.Fatal("canonical driver-link-then-execute fixdep bootstrap was not recognized")
	}
	searchedTemplate := strings.Replace(template, " -o ", " -L vendor/lib -lhelper -o ", 1)
	searchedRooted, err := compactKbuildRootedActionDirectRecipeText(profile, searchedTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if _, split := compactKbuildCmdAndFixdepRecipeSplit(
		target, match, compactKbuildFinalizeRootedActionRecipeText(searchedRooted), searchedRooted,
		compactKbuildAutomaticContext{target: target},
	); split {
		t.Fatal("driver-link fixdep bootstrap accepted an indirect library search")
	}

	// Exercise the selected-rule lowering path as well as the recognizer. The
	// family dependency analyzer can prune and share this bootstrap only when
	// the emitted atomic scriptrun retains its bounded compiler invocation.
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	linkRole, ok := toolaction.LinkContractRole("cc")
	if !ok {
		t.Fatal("cc has no semantic link contract")
	}
	baseRef := KbuildActionRoleRef{Scope: "host", Role: "cc"}
	linkRef := KbuildActionRoleRef{Scope: "host", Role: linkRole}
	metadata.actionRoles = append(slices.Clone(metadata.actionRoles), linkRef)
	metadata.actionContracts = map[KbuildActionRoleRef]CompactKbuildActionContract{
		baseRef: {},
		linkRef: {},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
		metadata: metadata,
	}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	match.rule = profile.Rules[0]
	match.commandTemplates = []CompactKbuildCommandTemplate{{Name: "hostcc", Text: template}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("prehost", "prehost", "sdk").
		forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, match, []compactKbuildRuleInput{{
		path: source, sourceID: sourceID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("node count = %d, want one atomic fixdep bootstrap: %#v", got, plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing fixdep bootstrap producer %q", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Kind != "compile" || node.Tool != compactKbuildScriptRunnerRole ||
		recipe.Tool != compactKbuildScriptRunnerRole || recipe.CompilerInvocation == nil ||
		recipe.CompilerInvocation.Tool != "cc" {
		t.Fatalf("fixdep bootstrap = %#v recipe = %#v, want typed atomic compiler scriptrun", node, recipe)
	}
	compilerArguments := strings.Join(recipe.CompilerInvocation.Arguments, "\x00")
	if !strings.Contains(compilerArguments, depfile) || !strings.Contains(compilerArguments, source) {
		t.Fatalf("fixdep compiler invocation omits depfile or source: %#v", recipe.CompilerInvocation)
	}
	if slices.Contains(recipe.CompilerInvocation.Arguments, "-c") ||
		toolaction.InvocationContractRole("cc", recipe.CompilerInvocation.Arguments) != linkRole ||
		!toolaction.CompilerInvocationProducesBinaryOutput("cc", recipe.CompilerInvocation.Arguments) {
		t.Fatalf("fixdep compiler invocation = %#v, want binary %s without -c", recipe.CompilerInvocation, linkRole)
	}
	if !recipe.CompilerInvocation.WorkingInputUsesComplete {
		t.Fatalf("fixdep driver-link working-input projection is incomplete: %#v", recipe.CompilerInvocation)
	}
	if len(recipe.ExecutableInputs) != 0 {
		t.Fatalf("self-bootstrap unexpectedly declares its output executable as an input: %#v", recipe.ExecutableInputs)
	}
	for _, pathname := range recipe.WorkingInputs {
		if pathname == target {
			t.Fatalf("self-bootstrap stages its output as an input: %#v", recipe.WorkingInputs)
		}
	}
	hasTarget, hasCommand, hasDepfile := false, false, false
	for _, pathname := range recipe.WorkingOutputs {
		hasTarget = hasTarget || pathname == target
		hasCommand = hasCommand || pathname == cmdfile
		hasDepfile = hasDepfile || pathname == depfile
	}
	if !hasTarget || !hasCommand || hasDepfile {
		t.Fatalf("fixdep bootstrap outputs = %#v, want target and command record but no removed depfile", recipe.WorkingOutputs)
	}

	selection := compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "prehost"}
	plan.selectionGraph = &compactKbuildSelectionGraph{
		profiles:                    map[string]CompactKbuildProfile{profile.Name: profile},
		selections:                  map[compactKbuildSelectionKey]CompactKbuildSelection{selection: {Profile: profile.Name}},
		materializedProducers:       map[compactKbuildSelectionKey]string{selection: node.ID},
		selectionInitialArtifacts:   map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{},
		selectionGeneratedArtifacts: map[compactKbuildSelectionKey][]CompactKbuildVisibleArtifact{},
	}
	dependencies, err := AnalyzeActionPlanNodeConfigDependencies(plan, node)
	if err != nil {
		t.Fatal(err)
	}
	wantSourcePaths := []string{source, nestedHeader, xallocHeader}
	slices.Sort(wantSourcePaths)
	if dependencies.Opaque || len(dependencies.Symbols) != 0 ||
		!slices.Equal(dependencies.SourcePaths, wantSourcePaths) ||
		slices.Contains(dependencies.SourcePaths, unrelatedSource) {
		t.Fatalf("fixdep driver-link dependencies = %#v, want config-independent source closure", dependencies)
	}
	plan.Products = []ActionPlanProduct{{Name: "sdk", Tree: "prehost", Path: target}}
	baseSnapshot, err := canonicalActionPlanSnapshot(
		plan, map[string]ConfigDependencySet{node.ID: dependencies}, familyTestConfig("y", "n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	overlaySnapshot, err := canonicalActionPlanSnapshot(
		plan, map[string]ConfigDependencySet{node.ID: dependencies}, familyTestConfig("y", "m"),
	)
	if err != nil {
		t.Fatal(err)
	}
	family, err := BuildActionPlanFamily([]ActionPlanFamilyVariant{
		{Name: "base", Snapshot: baseSnapshot},
		{Name: "irrelevant", Snapshot: overlaySnapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	shared := 0
	for _, familyNode := range family.Nodes {
		familyRecipe := family.Recipes[familyNode.Recipe]
		if familyNode.Kind == "compile" && familyRecipe.CompilerInvocation != nil &&
			slices.Equal(family.Memberships[familyNode.ID], []string{"base", "irrelevant"}) {
			shared++
			manifest, err := familyNodeSourceProjections(familyNode)
			if err != nil {
				t.Fatal(err)
			}
			projected := map[string]bool{}
			for _, binding := range manifest.Bindings {
				if binding.Tree == "kernel" {
					projected[binding.Path] = true
				}
			}
			for _, pathname := range wantSourcePaths {
				if !projected[pathname] {
					t.Fatalf("fixdep family source projection omits %q: %#v", pathname, manifest)
				}
			}
			if projected[unrelatedSource] {
				t.Fatalf("fixdep family source projection includes unrelated source %q: %#v", unrelatedSource, manifest)
			}
		}
	}
	if shared != 1 {
		t.Fatalf("fixdep driver-link family has %d shared compiles, want one: memberships=%#v nodes=%#v", shared, family.Memberships, family.Nodes)
	}

	sourceWriter := strings.Replace(template, objectRoot+target+" "+objectRoot+depfile, "__LINUX_BZL_SOURCE_TREE__/"+target+" "+objectRoot+depfile, 1)
	sourceRooted, err := compactKbuildRootedActionDirectRecipeText(profile, sourceWriter)
	if err != nil {
		t.Fatal(err)
	}
	if _, split := compactKbuildCmdAndFixdepRecipeSplit(
		target, match, compactKbuildFinalizeRootedActionRecipeText(sourceRooted), sourceRooted,
		compactKbuildAutomaticContext{target: target},
	); split {
		t.Fatal("canonical fixdep bootstrap accepted a source-tree executable")
	}
}

func TestHermeticKbuildCompilerFallbackFinalizesImmutableSourceProvenance(t *testing.T) {
	const (
		target = "tools/objtool/fixdep"
		source = "tools/build/fixdep.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "host:objtool", "tools/objtool/Makefile", "tools/objtool", `
tools/objtool/fixdep: tools/build/fixdep.c FORCE
	@true
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("host", "host", "sdk")
	template := KbuildActionRoleToken("host", "cc") +
		" -c -o " + compactKbuildActionObjectTreeMarker + "/" + target + " " + source
	producer, err := builder.buildHermeticKbuildScriptContext(
		target,
		compactKbuildRuleMatch{profile: profile, rule: profile.Rules[0]},
		[]compactKbuildRuleInput{{path: source, sourceID: sourceID}},
		template,
		nil,
		nil,
		compactKbuildHermeticScriptOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("compiler fallback producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if !strings.Contains(script, "'${tree:kernel}/"+source+"'") {
		t.Fatalf("compiler fallback lost immutable source provenance: %q", script)
	}
	if strings.Contains(script, compactKbuildActionSourceTreeMarker) || strings.Contains(script, compactKbuildActionObjectTreeMarker) {
		t.Fatalf("compiler fallback leaked a private action marker: %q", script)
	}
	if !slices.Contains(node.Trees, "kernel") || !slices.Contains(recipe.Trees, "kernel") {
		t.Fatalf("compiler fallback omits kernel tree binding: node=%#v recipe=%#v", node.Trees, recipe.Trees)
	}
}

func TestHermeticKbuildSingleDriverLinkIsTypedCompile(t *testing.T) {
	const (
		target  = "scripts/basic/fixdep"
		source  = "scripts/basic/fixdep.c"
		ambient = "include/config/kernel.release"
	)
	profile := mustCompactKbuildProfileForTest(t, "host:fixdep-link", "scripts/Makefile.host", "", `
scripts/basic/fixdep: scripts/basic/fixdep.c FORCE
	@true
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{}, metadata: metadata,
	}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	ambientID, err := ensureActionPlanSource(plan, "config", "kernel.release")
	if err != nil {
		t.Fatal(err)
	}
	template := compactKbuildRecipeLineShells([]string{
		KbuildActionRoleToken("host", "cc") + " -O2 -o " +
			compactKbuildActionObjectTreeMarker + "/" + target + " " +
			compactKbuildActionSourceTreeMarker + "/" + source,
	})
	commands, err := compactKbuildCompoundProgramCommands(template)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("host", "host", "sdk")
	producer, err := builder.buildHermeticKbuildCompound(
		target,
		compactKbuildRuleMatch{profile: profile, rule: profile.Rules[0]},
		[]compactKbuildRuleInput{
			{path: source, sourceID: sourceID},
			{path: ambient, sourceID: ambientID, objectTree: true, workingOnly: true},
		},
		template,
		commands,
	)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("fixdep driver-link producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Kind != "compile" || recipe.CompilerInvocation == nil ||
		recipe.CompilerInvocation.Tool != "cc" ||
		toolaction.InvocationContractRole("cc", recipe.CompilerInvocation.Arguments) != "cc-link" {
		t.Fatalf("fixdep driver-link = node %#v recipe %#v, want typed cc-link compile", node, recipe)
	}
	if !recipe.CompilerInvocation.WorkingInputUsesComplete {
		t.Fatalf("fixdep driver-link has incomplete working-input uses: %#v", recipe.CompilerInvocation)
	}
	used := map[string]bool{}
	for _, reference := range recipe.CompilerInvocation.WorkingInputUses {
		used[recipe.WorkingInputs[reference]] = true
	}
	if !used[source] || used[ambient] {
		t.Fatalf("fixdep driver-link working-input uses = %#v, want source only (working=%#v)", used, recipe.WorkingInputs)
	}
}

func TestHermeticKbuildSingleDriverLinkIndirectInputsKeepIncompleteProjection(t *testing.T) {
	const (
		target = "tools/hidden-link-input"
		source = "tools/hidden-link-input.c"
		hidden = "libs/libhidden.a"
	)
	for _, test := range []struct {
		name     string
		operands string
	}{
		{name: "unquoted glob", operands: " libs/*.a"},
		{name: "library search", operands: " -L libs -lhidden"},
		{name: "source-selected sysroot", operands: " --sysroot libs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := mustCompactKbuildProfileForTest(t, "host:hidden-link-input", "scripts/Makefile.host", "", `
tools/hidden-link-input: tools/hidden-link-input.c FORCE
	@true
`, nil)
			profile = compactKbuildProfileWithSourcesForTest(t, profile, source)
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: CompactKbuildInvocationObjectTree,
			}); err != nil {
				t.Fatal(err)
			}
			metadata := &CompactMetadata{
				actionRoles: testConfiguredScopedActionRoles,
				Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
			}
			plan := &ActionPlan{
				Toolsets: map[string]string{"host": actionPlanTestProbeIdentity},
				Recipes:  map[string]ActionRecipe{}, metadata: metadata,
			}
			hiddenNode := ActionPlanNode{
				Stage: "prehost", Kind: "generate", Tool: "actionfile", Product: "sdk",
				Outputs: []ActionPlanOutput{{Tree: "prehost", Path: hidden}},
			}
			hiddenRecipe := ActionRecipe{
				Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
				Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
			}
			hiddenProducer, err := appendActionPlanNode(plan, hiddenNode, hiddenRecipe)
			if err != nil {
				t.Fatal(err)
			}
			sourceID, err := metadata.ensureActionPlanSource(plan, source)
			if err != nil {
				t.Fatal(err)
			}
			template := compactKbuildRecipeLineShells([]string{
				KbuildActionRoleToken("host", "cc") + " " +
					compactKbuildActionSourceTreeMarker + "/" + source + test.operands + " -o " +
					compactKbuildActionObjectTreeMarker + "/" + target,
			})
			commands, err := compactKbuildCompoundProgramCommands(template)
			if err != nil {
				t.Fatal(err)
			}
			builder := newCompactKbuildRulePlanBuilder(metadata, plan).
				forProfile(profile).
				forOutput("host", "host", "sdk")
			producer, err := builder.buildHermeticKbuildCompound(
				target,
				compactKbuildRuleMatch{profile: profile, rule: profile.Rules[0]},
				[]compactKbuildRuleInput{
					{path: source, sourceID: sourceID},
					{path: hidden, producer: hiddenProducer},
				},
				template,
				commands,
			)
			if err != nil {
				t.Fatal(err)
			}
			node, ok := compactKbuildPlanNode(plan, producer)
			if !ok {
				t.Fatalf("hidden-input driver-link producer %q not found", producer)
			}
			recipe := plan.Recipes[node.Recipe]
			if node.Kind != "compile" || recipe.CompilerInvocation == nil ||
				recipe.CompilerInvocation.WorkingInputUsesComplete {
				t.Fatalf("hidden-input driver-link = node %#v recipe %#v, want typed incomplete projection", node, recipe)
			}
			stagedHidden := false
			for _, pathname := range recipe.WorkingInputs {
				stagedHidden = stagedHidden || pathname == hidden
			}
			if !stagedHidden {
				t.Fatalf("hidden input %q was not staged for incomplete fallback: %#v", hidden, recipe.WorkingInputs)
			}
		})
	}
}

func TestHermeticKbuildCompoundBindsNamespacedCompilerSourceExactly(t *testing.T) {
	const (
		rustRoot = "external/rules_rs++toolchains+rust_src_1_97_0/lib/rustlib/src/library"
		source   = rustRoot + "/core/src/lib.rs"
		target   = "rust/core.o"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:rust-core", "scripts/Makefile.build", "", `
objtree := .
cmd_rustc_library = { RELATIVE_OBJTREE=$(objtree) OBJTREE=$(abspath $(objtree)) $(RUSTC) --crate-name core --emit=obj=$@ $<; $(OBJCOPY) --strip-debug $@; }
rust/core.o: $(RUST_LIB_SRC)/core/src/lib.rs FORCE
	$(call if_changed,rustc_library)
FORCE:
`, map[string]string{
		"OBJCOPY":      KbuildActionRoleToken("target", "objcopy"),
		"RUSTC":        KbuildActionRoleToken("target", "rustc"),
		"RUST_LIB_SRC": rustRoot,
	})
	kernelRoot := t.TempDir()
	objectRoot := t.TempDir()
	physicalRustRoot := t.TempDir()
	mustWriteSource(t, kernelRoot, source, "wrong kernel shadow\n")
	mustWriteSource(t, physicalRustRoot, "core/src/lib.rs", "#![no_std]\n")
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__": kernelRoot,
		"__LINUX_BZL_OBJECT_TREE__": objectRoot,
		rustRoot:                    physicalRustRoot,
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles:      testTargetActionRoles(append(testConfiguredActionRoles, "rustc")...),
		sourceNamespaces: map[string]string{rustRoot: "rust"},
		Config:           CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("target", "objects", "sdk")
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("namespaced compiler producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("namespaced compiler producer = %#v recipe = %#v, want scriptrun", node, recipe)
	}
	sourceID := ""
	for _, candidate := range plan.Sources {
		if candidate.Namespace == "rust" && candidate.Path == source {
			sourceID = candidate.ID
			break
		}
	}
	if sourceID == "" {
		t.Fatalf("namespaced compiler sources = %#v, want exact rust source %q", plan.Sources, source)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if !strings.Contains(script, "OBJTREE=${tree:prep}") || strings.Contains(script, " OBJTREE=.") {
		t.Fatalf("namespaced compiler script lost absolute object-root provenance: %q", script)
	}
	if !strings.Contains(script, "RELATIVE_OBJTREE=.") || strings.Contains(script, "RELATIVE_OBJTREE=${tree:prep}") {
		t.Fatalf("namespaced compiler script made an ordinary object-root value absolute: %q", script)
	}
	if !slices.Contains(recipe.Arguments, "prep=${work:root}") {
		t.Fatalf("namespaced compiler absolute object root is not bound to private writable state: arguments=%#v", recipe.Arguments)
	}
	if slices.Contains(node.Trees, "prep") || slices.Contains(recipe.Trees, "prep") {
		t.Fatalf("namespaced compiler absolute object root retained an immutable prep tree: node=%#v recipe=%#v", node.Trees, recipe.Trees)
	}
	environmentName := compactKbuildActionSourceInputEnvironment(sourceID)
	if !strings.Contains(script, `"$`+environmentName+`"`) {
		t.Fatalf("namespaced compiler script omits exact source environment %q: %q", environmentName, script)
	}
	for _, rejected := range []string{source, "${tree:kernel}", compactKbuildActionSourceInputPrefix} {
		if strings.Contains(script, rejected) {
			t.Fatalf("namespaced compiler script retained %q: %q", rejected, script)
		}
	}
	binding := recipe.Environment[environmentName]
	if !strings.HasPrefix(binding, "${source:") || !strings.HasSuffix(binding, "}") {
		t.Fatalf("namespaced compiler environment %s=%q, want exact source binding", environmentName, binding)
	}
	sourceKey := strings.TrimSuffix(strings.TrimPrefix(binding, "${source:"), "}")
	if !slices.Contains(recipe.Sources, sourceKey) {
		t.Fatalf("namespaced compiler source binding %q is absent from recipe sources %#v", sourceKey, recipe.Sources)
	}
	if got := recipe.WorkingInputs["source:"+sourceKey]; got != source {
		t.Fatalf("namespaced compiler source working path = %q, want %q", got, source)
	}
	if slices.Contains(node.Trees, "kernel") || slices.Contains(recipe.Trees, "kernel") {
		t.Fatalf("namespaced compiler gained a kernel tree input: node=%#v recipe=%#v", node.Trees, recipe.Trees)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("namespaced compiler action plan is invalid: %v", err)
	}
}

func TestCompactKbuildRecipeSideEffectProjectionPartitionsCompositeGroup(t *testing.T) {
	const target = "generated/result"
	context := compactKbuildAutomaticContext{target: target}
	for name, test := range map[string]struct {
		template string
		programs []string
		outputs  []string
	}{
		"group then ordinary command": {
			template: `{ printf payload | cat; printf suffix; } > $@; printf '%s\n' saved > generated/.result.cmd`,
			programs: []string{":", "printf"},
			outputs:  []string{target, "generated/.result.cmd"},
		},
		"quoted closing brace is data": {
			template: `{ printf payload; '}' > generated/ignored.cmd; printf suffix; } >> generated/result.cmd; touch generated/done > generated/after.cmd`,
			programs: []string{":", "touch"},
			outputs:  []string{"generated/result.cmd", "generated/after.cmd"},
		},
		"quoted brace argument stays ordinary": {
			template: `printf '{' > generated/literal.txt; printf saved > generated/state.cmd`,
			programs: []string{"printf", "printf"},
			outputs:  []string{"generated/literal.txt", "generated/state.cmd"},
		},
		"literal hashes stay ordinary data": {
			template: `printf foo#bar '# quoted ; hash' "# double-quoted ; hash" \#escaped > generated/literal.txt; printf saved > generated/state.cmd`,
			programs: []string{"printf", "printf"},
			outputs:  []string{"generated/literal.txt", "generated/state.cmd"},
		},
		"and chain before ordinary command": {
			template: `printf conditional > generated/conditional.cmd && test -s generated/conditional.cmd; printf saved > generated/state.cmd`,
			programs: []string{"printf"},
			outputs:  []string{"generated/state.cmd"},
		},
		"or chain before ordinary command": {
			template: `false || printf conditional > generated/conditional.cmd; printf saved > generated/state.cmd`,
			programs: []string{"printf"},
			outputs:  []string{"generated/state.cmd"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			commands := compactKbuildRecipeSideEffectProjection([]string{test.template}, context)
			programs := make([]string, 0, len(commands))
			outputs := make([]string, 0, len(commands))
			for _, command := range commands {
				programs = append(programs, command.program)
				outputs = append(outputs, command.stdout)
			}
			if !slices.Equal(programs, test.programs) || !slices.Equal(outputs, test.outputs) {
				t.Fatalf("projection programs=%q outputs=%q, want programs=%q outputs=%q", programs, outputs, test.programs, test.outputs)
			}
		})
	}
}

func TestCompactKbuildRecipeSideEffectProjectionPreservesStubcopySavecmdAfterIf(t *testing.T) {
	const (
		target      = "drivers/firmware/efi/libstub/alignedmem.stub.o"
		input       = "drivers/firmware/efi/libstub/alignedmem.o"
		commandFile = "drivers/firmware/efi/libstub/.alignedmem.stub.o.cmd"
	)
	context := compactKbuildAutomaticContext{target: target, normal: []string{input}}
	template := `strip --strip-debug -o ` + target + ` ` + input + `; ` +
		`if objdump -r ` + target + ` | grep R_AARCH64_ABS; then ` +
		`echo "` + target + `: absolute symbol references not allowed in the EFI stub" >&2; /bin/false; fi; ` +
		`objcopy --remove-section=.note.gnu.property --prefix-alloc-sections=.init ` +
		`--prefix-symbols=__efistub_ ` + input + ` ` + target + `; ` +
		`printf '%s\n' 'savedcmd_alignedmem.stub.o := stubcopy' > ` + commandFile
	units, ok := compactKbuildTopLevelSemicolonUnits(template)
	if !ok {
		t.Fatal("stubcopy recipe did not partition into bounded top-level units")
	}
	if len(units) != 4 || units[0].conditional || !units[1].conditional || units[2].conditional || units[3].conditional {
		t.Fatalf("stubcopy top-level units=%#v, want only if/fi unit conditional", units)
	}
	for index, unit := range units {
		if unit.conditional {
			continue
		}
		if _, err := parseCompactKbuildRecipe(unit.value, context); err != nil {
			t.Fatalf("stubcopy unconditional unit %d %q: %v", index, unit.value, err)
		}
	}

	commands := compactKbuildRecipeSideEffectProjection([]string{template}, context)
	programs := make([]string, 0, len(commands))
	for _, command := range commands {
		programs = append(programs, command.program)
	}
	if want := []string{"strip", "objcopy", "printf"}; !slices.Equal(programs, want) {
		t.Fatalf("stubcopy projection programs=%q, want %q", programs, want)
	}
	outputs, err := compactKbuildCompoundSurvivingExplicitOutputs(
		CompactKbuildProfile{}, commands, target,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{commandFile}; !slices.Equal(outputs, want) {
		t.Fatalf("stubcopy surviving outputs=%q, want %q", outputs, want)
	}
}

func TestCompactKbuildRecipeSideEffectProjectionIfControlFlowFailsClosed(t *testing.T) {
	const state = "generated/state.cmd"
	context := compactKbuildAutomaticContext{target: "generated/result"}
	for name, test := range map[string]struct {
		template string
		outputs  []string
	}{
		"branch output is conditional": {
			template: `if true; then printf branch > generated/branch.cmd; fi`,
		},
		"unconditional sibling survives": {
			template: `if true; then printf branch > generated/branch.cmd; fi; printf saved > ` + state,
			outputs:  []string{state},
		},
		"nested branch output is conditional": {
			template: `if true; then if false; then printf branch > generated/branch.cmd; fi; fi; printf saved > ` + state,
			outputs:  []string{state},
		},
		"unclosed if": {
			template: `if true; then printf branch > generated/branch.cmd; printf saved > ` + state,
		},
		"unmatched fi": {
			template: `fi; printf saved > ` + state,
		},
		"extra fi": {
			template: `if true; then printf branch > generated/branch.cmd; fi; fi; printf saved > ` + state,
		},
	} {
		t.Run(name, func(t *testing.T) {
			commands := compactKbuildRecipeSideEffectProjection([]string{test.template}, context)
			outputs := make([]string, 0, len(commands))
			for _, command := range commands {
				if command.stdout != "" {
					outputs = append(outputs, command.stdout)
				}
			}
			if !slices.Equal(outputs, test.outputs) {
				t.Fatalf("if projection outputs=%q, want %q", outputs, test.outputs)
			}
		})
	}
}

func TestCompactKbuildRecipeSideEffectProjectionRejectsUnsupportedCompositeUnit(t *testing.T) {
	for name, template := range map[string]string{
		"top-level and":             `{ printf x; } > generated/first.cmd && printf y > generated/second.cmd`,
		"top-level or":              `{ printf x; } > generated/first.cmd || printf y > generated/second.cmd`,
		"top-level pipeline":        `printf x | printf y > generated/first.cmd; printf z > generated/second.cmd`,
		"top-level background":      `{ printf x; } > generated/first.cmd & printf y > generated/second.cmd`,
		"top-level parentheses":     `( { printf x; } > generated/first.cmd ); printf y > generated/second.cmd`,
		"unbalanced group":          `{ printf x; printf y > generated/second.cmd`,
		"group closes after and":    `{ printf x && } > generated/first.cmd; printf y > generated/second.cmd`,
		"extra group operand":       `{ printf x; } > generated/first.cmd extra; printf y > generated/second.cmd`,
		"unsupported ordinary unit": `{ printf x; } > generated/first.cmd; printf y >> generated/second.cmd`,
		"quoted opening brace":      `'{'; printf y > generated/second.cmd`,
		"escaped opening brace":     `\{; printf y > generated/second.cmd`,
		"unbalanced closing brace":  `}; printf y > generated/second.cmd`,
		"comment hides separator":   `true # ; printf y > generated/second.cmd`,
		"comment after group":       `{ printf x; } > generated/first.cmd; true # ; printf y > generated/second.cmd`,
		"glob group output":         `{ printf x; } > generated/*.cmd; printf y > generated/second.cmd`,
		"tilde group output":        `{ printf x; } > ~/generated.cmd; printf y > generated/second.cmd`,
		"quoted backslash output":   `{ printf x; } > "generated/foo\q.cmd"; printf y > generated/second.cmd`,
	} {
		t.Run(name, func(t *testing.T) {
			if commands := compactKbuildRecipeSideEffectProjection(
				[]string{template},
				compactKbuildAutomaticContext{target: "generated/result"},
			); len(commands) != 0 {
				t.Fatalf("unsupported composite projection=%#v, want no partial side effects", commands)
			}
		})
	}
}

func TestHermeticKbuildCompoundVersionsSelectionSideOutputs(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "selection-side-outputs", "scripts/Makefile.build", "", `
first.out: FORCE
	@true
second.out: FORCE
	@true
observed.out: FORCE
	@true
`, nil)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity, "host": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
		metadata: metadata,
	}
	physicalSideOutputs := []string{}
	for index, target := range []string{"first.out", "second.out"} {
		rootedTarget := compactKbuildActionObjectTreeMarker + "/" + target
		rootedSideOutput := compactKbuildActionObjectTreeMarker + "/state/shared.cmd"
		lines := []string{
			"touch " + rootedTarget + "; printf saved > " + rootedSideOutput,
			"{ printf more; } >> " + rootedSideOutput,
		}
		template := compactKbuildRecipeLineShells(lines)
		commands, err := compactKbuildCompoundProgramCommands(template)
		if err != nil {
			t.Fatal(err)
		}
		sideEffectCommands := compactKbuildRecipeSideEffectProjection(
			lines,
			compactKbuildAutomaticContext{target: target},
		)
		key := compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "target"}
		builder := newCompactKbuildRulePlanBuilder(metadata, plan).
			forSelection(key, profile).
			forOutput("target", "objects", "vmlinux")
		if index == 0 {
			builder, err = builder.forObservedOutputs(target, []compactKbuildObservedOutput{{
				output: ActionPlanOutput{Tree: "metadata", Path: ".captures/first-state"},
				path:   "state/opaque",
			}})
			if err != nil {
				t.Fatal(err)
			}
		}
		producer, err := builder.buildHermeticKbuildCompoundWithSideEffects(target, compactKbuildRuleMatch{
			profile: profile,
			rule:    profile.Rules[index],
		}, nil, template, commands, sideEffectCommands)
		if err != nil {
			t.Fatal(err)
		}
		node, ok := compactKbuildPlanNode(plan, producer)
		if !ok {
			t.Fatalf("missing selection producer %q", producer)
		}
		recipe := plan.Recipes[node.Recipe]
		sideSlot := -1
		observedSlot := -1
		for slot, output := range node.Outputs {
			if output.Path == "state/shared.cmd" {
				sideSlot = slot
				physicalSideOutputs = append(physicalSideOutputs, actionPlanOutputArtifactPath(output))
			}
			if output.ObservedPath == "state/opaque" {
				observedSlot = slot
			}
		}
		if sideSlot != 1 || recipe.WorkingOutputs[planOrdinal(sideSlot)] != "state/shared.cmd" {
			t.Fatalf("selection %q side-output order/binding: node=%#v recipe=%#v", target, node, recipe)
		}
		if index == 0 && (observedSlot != 2 || recipe.ObservedOutputs[planOrdinal(observedSlot)] != "state/opaque") {
			t.Fatalf("selection %q observed-output order/binding: node=%#v recipe=%#v", target, node, recipe)
		}
	}
	if len(physicalSideOutputs) != 2 || physicalSideOutputs[0] == physicalSideOutputs[1] {
		t.Fatalf("selection side-output artifacts = %q, want two immutable physical paths", physicalSideOutputs)
	}
	for _, artifact := range physicalSideOutputs {
		if !strings.HasPrefix(artifact, ".linux-bzl-side-outputs/") {
			t.Fatalf("selection side-output artifact = %q, want reserved immutable namespace", artifact)
		}
	}

	const observedTarget = "observed.out"
	rootedObservedTarget := compactKbuildActionObjectTreeMarker + "/" + observedTarget
	rootedObservedSideOutput := compactKbuildActionObjectTreeMarker + "/state/shared.cmd"
	observedLines := []string{
		"touch " + rootedObservedTarget + "; printf saved > " + rootedObservedSideOutput,
		"{ printf more; } >> " + rootedObservedSideOutput,
	}
	observedTemplate := compactKbuildRecipeLineShells(observedLines)
	observedCommands, err := compactKbuildCompoundProgramCommands(observedTemplate)
	if err != nil {
		t.Fatal(err)
	}
	observedSideEffectCommands := compactKbuildRecipeSideEffectProjection(
		observedLines,
		compactKbuildAutomaticContext{target: observedTarget},
	)
	observedBuilder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forSelection(compactKbuildSelectionKey{
			profile: profile.Name, target: observedTarget, stage: "target",
		}, profile).
		forOutput("target", "objects", "vmlinux")
	observedBuilder, err = observedBuilder.forObservedOutputs(observedTarget, []compactKbuildObservedOutput{{
		output: ActionPlanOutput{Tree: "metadata", Path: ".captures/shared-command"},
		path:   "state/shared.cmd",
	}})
	if err != nil {
		t.Fatal(err)
	}
	observedProducer, err := observedBuilder.buildHermeticKbuildCompoundWithSideEffects(observedTarget, compactKbuildRuleMatch{
		profile: profile,
		rule:    profile.Rules[2],
	}, nil, observedTemplate, observedCommands, observedSideEffectCommands)
	if err != nil {
		t.Fatal(err)
	}
	observedNode, ok := compactKbuildPlanNode(plan, observedProducer)
	if !ok {
		t.Fatalf("missing observed selection producer %q", observedProducer)
	}
	observedRecipe := plan.Recipes[observedNode.Recipe]
	if len(observedNode.Outputs) != 2 || observedNode.Outputs[1].ObservedPath != "state/shared.cmd" ||
		observedRecipe.ObservedOutputs["00000001"] != "state/shared.cmd" ||
		observedRecipe.WorkingOutputs["00000001"] != "" {
		t.Fatalf("observation-owned side output was also directly published: node=%#v recipe=%#v", observedNode, observedRecipe)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("selection side-output plan is invalid: %v", err)
	}
}

func TestGenericKbuildRecipeChainsObjcopyThroughInPlaceTarget(t *testing.T) {
	const target = "vmlinux"
	profile := mustCompactKbuildProfileForTest(t, "vmlinux", "scripts/Makefile.vmlinux", "", `
cmd_strip_relocs = $(OBJCOPY) --set-section-flags .rel=noload $< $@; $(OBJCOPY) --remove-section=.rel $@
`, map[string]string{"OBJCOPY": KbuildActionRoleToken("target", "objcopy")})
	metadata := &CompactMetadata{actionRoles: testConfiguredScopedActionRoles}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	seed := ActionPlanNode{
		Stage: "target", Kind: "link-relocatable", Tool: "ld", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "vmlinux", Path: "vmlinux.unstripped"}},
	}
	seedRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "link-relocatable", Tool: "ld",
		Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	inputProducer, err := appendActionPlanNode(plan, seed, seedRecipe)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: target, command: "strip_relocs",
	}, []compactKbuildRuleInput{{path: "vmlinux.unstripped", producer: inputProducer}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 3; got != want {
		t.Fatalf("node count=%d, want input + two objcopy actions: %#v", got, plan.Nodes)
	}
	first, second := plan.Nodes[1], plan.Nodes[2]
	if first.Tool != "objcopy" || first.Outputs[0].Path != target ||
		!strings.HasPrefix(first.Outputs[0].ArtifactPath, ".linux-bzl-intermediate/") ||
		actionPlanOutputIsCanonical(first.Outputs[0]) || first.Inputs[0].ProducerID != inputProducer {
		t.Fatalf("first objcopy=%#v", first)
	}
	secondRecipe := plan.Recipes[second.Recipe]
	if producer != second.ID || second.Tool != "objcopy" || second.Outputs[0].Path != target ||
		secondRecipe.WorkingInputs["input:prerequisite:00000001"] != target ||
		secondRecipe.WorkingOutputs["00000000"] != target || !slices.Contains(secondRecipe.Arguments, target) {
		t.Fatalf("in-place objcopy=%#v recipe=%#v", second, secondRecipe)
	}
}

func TestGenericKbuildRecipeChainsInPlaceTargetAfterInitialSnapshot(t *testing.T) {
	const target = "rust/compiler_builtins.o"
	artifact := CompactKbuildVisibleArtifact{
		Path: target, Profile: "build:rust-prep", Target: target,
	}
	profile := mustCompactKbuildProfileForTest(t, "build:rust-target", "scripts/Makefile.build", "", `
cmd_postprocess = $(RUSTC) --emit=obj=$@ $<; $(OBJCOPY) --remove-section=.discard $@
`, map[string]string{
		"RUSTC":   KbuildActionRoleToken("target", "rustc"),
		"OBJCOPY": KbuildActionRoleToken("target", "objcopy"),
	})
	setTestCompactKbuildInitialVisibleArtifacts(t, &profile, []CompactKbuildVisibleArtifact{artifact})
	prepProfile := CompactKbuildProfile{
		Name: "build:rust-prep", Path: "scripts/Makefile.build", EntryTargets: []string{target},
	}
	targetSelection := CompactKbuildSelection{
		Profile: profile.Name, Target: target, MakeTarget: target, Lifecycle: "target", Scope: "target", Stage: "target",
		UsesInitialObjectTree: true,
		InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts(
			[]CompactKbuildVisibleArtifact{artifact},
		),
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{prepProfile, profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: prepProfile.Name, Target: target, MakeTarget: target, Lifecycle: "prep", Scope: "target", Stage: "prep"},
			targetSelection,
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	seed := func(stage, tree, output string) string {
		t.Helper()
		node := ActionPlanNode{
			Stage: stage, Kind: "generate", Tool: "actionfile", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: tree, Path: output}},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
			Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
		}
		producer, appendErr := appendActionPlanNode(plan, node, recipe)
		if appendErr != nil {
			t.Fatal(appendErr)
		}
		return producer
	}
	const source = "rust/compiler_builtins.rs"
	inputProducer := seed("target", "objects", source)
	prepProducer := seed("prep", "prep", target)
	prepKey := compactKbuildSelectionKey{profile: prepProfile.Name, target: target, stage: "prep"}
	if err := graph.recordMaterializedProducer(prepKey, prepProducer); err != nil {
		t.Fatal(err)
	}
	targetKey := compactKbuildSelectionKey{profile: profile.Name, target: target, stage: "target"}
	builder := newCompactKbuildRulePlanBuilder(
		&CompactMetadata{
			actionRoles: testScopedActionRoles(append(testConfiguredActionRoles, "rustc")...),
		}, plan,
	).
		withSelectionGraph(graph).
		forSelection(targetKey, profile).
		forOutput("target", "objects", "vmlinux").
		withInitialObjectTree(true, artifact)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: target, command: "postprocess",
	}, []compactKbuildRuleInput{{path: source, producer: inputProducer}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 4; got != want {
		t.Fatalf("node count=%d, want source + prep snapshot + rustc + objcopy: %#v", got, plan.Nodes)
	}
	first, second := plan.Nodes[2], plan.Nodes[3]
	if producer != second.ID || first.Tool != "rustc" || second.Tool != "objcopy" ||
		first.Outputs[0].Path != target || second.Outputs[0].Path != target {
		t.Fatalf("rustc/objcopy chain first=%#v second=%#v producer=%q", first, second, producer)
	}
	secondRecipe := plan.Recipes[second.Recipe]
	targetInputProducer := ""
	targetInputSlot := -1
	for index, key := range secondRecipe.Inputs {
		if secondRecipe.WorkingInputs["input:"+key] == target {
			targetInputProducer = second.Inputs[index].ProducerID
			targetInputSlot = second.Inputs[index].Slot
			break
		}
	}
	if targetInputProducer != first.ID || targetInputSlot != 0 {
		t.Fatalf(
			"final objcopy stages %q from %q slot %d, want recipe-local rustc %q slot 0 (prep snapshot %q)",
			target, targetInputProducer, targetInputSlot, first.ID, prepProducer,
		)
	}
}

func TestGenericKbuildRecipeLowersVmlinuxObjectLinkAndObjtool(t *testing.T) {
	const (
		target  = "vmlinux.o"
		objtool = "tools/objtool/objtool"
	)
	profile := mustCompactKbuildProfileForTest(t, "vmlinux-o", "scripts/Makefile.vmlinux_o", "", `
cmd_objtool = ; $(objtool) --link $@
cmd_ld_vmlinux.o = $(LD) $(KBUILD_LDFLAGS) -r -o $@ $(vmlinux-o-ld-args-y) $(addprefix -T ,$(initcalls-lds)) --whole-archive vmlinux.a --no-whole-archive --start-group $(KBUILD_VMLINUX_LIBS) --end-group $(cmd_objtool)
`, map[string]string{
		"LD": KbuildActionRoleToken("target", "ld"), "objtool": objtool,
		"KBUILD_LDFLAGS": "--exact-kbuild", "vmlinux-o-ld-args-y": "--exact-vmlinux-o",
		"initcalls-lds": "scripts/initcalls.lds", "KBUILD_VMLINUX_LIBS": "lib/built-in.a",
	})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{actionRoles: testConfiguredScopedActionRoles}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	seed := func(stage, tree, output string) string {
		t.Helper()
		node := ActionPlanNode{
			Stage: stage, Kind: "generate", Tool: "actionfile", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: tree, Path: output}},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
			Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
		}
		producer, err := appendActionPlanNode(plan, node, recipe)
		if err != nil {
			t.Fatal(err)
		}
		return producer
	}
	objtoolProducer := seed("host", "host", objtool)
	inputs := []compactKbuildRuleInput{}
	for _, input := range []string{"vmlinux.a", "scripts/initcalls.lds", "lib/built-in.a"} {
		inputs = append(inputs, compactKbuildRuleInput{path: input, producer: seed("target", "objects", input)})
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: target, command: "ld_vmlinux.o",
	}, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 6; got != want {
		t.Fatalf("node count=%d, want four inputs + link + objtool: %#v", got, plan.Nodes)
	}
	link, mutate := plan.Nodes[4], plan.Nodes[5]
	linkRecipe := plan.Recipes[link.Recipe]
	if link.Tool != "ld" || link.Kind != "link-relocatable" || link.Outputs[0].Path != target ||
		!strings.HasPrefix(link.Outputs[0].ArtifactPath, ".linux-bzl-intermediate/") ||
		actionPlanOutputIsCanonical(link.Outputs[0]) {
		t.Fatalf("vmlinux.o link=%#v", link)
	}
	joined := strings.Join(linkRecipe.Arguments, " ")
	for _, exact := range []string{"--exact-kbuild", "--exact-vmlinux-o", "--whole-archive", "--start-group"} {
		if !strings.Contains(joined, exact) {
			t.Fatalf("link arguments omit %q: %q", exact, linkRecipe.Arguments)
		}
	}
	linkerScriptFlag := slices.Index(linkRecipe.Arguments, "-T")
	if linkerScriptFlag < 0 || linkerScriptFlag+1 >= len(linkRecipe.Arguments) ||
		!strings.HasPrefix(linkRecipe.Arguments[linkerScriptFlag+1], "${input:") {
		t.Fatalf("link arguments do not preserve GNU addprefix's split -T operand: %q", linkRecipe.Arguments)
	}
	for _, input := range []string{"vmlinux.a", "scripts/initcalls.lds", "lib/built-in.a"} {
		if !slices.Contains(sortedStringMapValues(linkRecipe.WorkingInputs), input) {
			t.Fatalf("link working inputs omit %q: %#v", input, linkRecipe.WorkingInputs)
		}
	}
	mutateRecipe := plan.Recipes[mutate.Recipe]
	if producer != mutate.ID || mutate.Tool != "generated" || mutate.Outputs[0].Path != target ||
		mutateRecipe.WorkingOutputs["00000000"] != target || !slices.Contains(mutateRecipe.Arguments, "--link") ||
		!slices.ContainsFunc(mutate.Inputs, func(input ActionPlanNodeEdge) bool { return input.ProducerID == objtoolProducer }) {
		t.Fatalf("vmlinux.o objtool=%#v recipe=%#v", mutate, mutateRecipe)
	}
}

func TestSimpleRelocsCommandUsesGenericGeneratedProgram(t *testing.T) {
	const (
		target  = "arch/x86/realmode/rm/realmode.relocs"
		input   = "arch/x86/realmode/rm/realmode.elf"
		program = "arch/x86/tools/relocs"
	)
	profile := mustCompactKbuildProfileForTest(t, "realmode", "arch/x86/realmode/rm/Makefile", "arch/x86/realmode/rm", `
cmd_relocs = $(CMD_RELOCS) --realmode $< > $@
$(obj)/realmode.relocs: $(obj)/realmode.elf FORCE
		$(call if_changed,relocs)
`, map[string]string{"CMD_RELOCS": "__LINUX_BZL_OBJECT_TREE__/" + program, "obj": "arch/x86/realmode/rm"})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "arch/x86/realmode/rm",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	seed := func(stage, tree, output string) string {
		t.Helper()
		node := ActionPlanNode{
			Stage: stage, Kind: "generate", Tool: "actionfile", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: tree, Path: output}},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
			Arguments: []string{"-out", "${output:00000000}", "-content_base64", ""}, Outputs: []string{"00000000"},
		}
		producer, err := appendActionPlanNode(plan, node, recipe)
		if err != nil {
			t.Fatal(err)
		}
		return producer
	}
	inputProducer := seed("target", "objects", input)
	programProducer := seed("host", "host", program)
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok || node.Tool != "generated" || node.Outputs[0].Path != target {
		t.Fatalf("relocs node=%#v", node)
	}
	recipe := plan.Recipes[node.Recipe]
	if recipe.Stdout != "00000000" || !slices.Contains(recipe.Arguments, "--realmode") ||
		!slices.ContainsFunc(node.Inputs, func(edge ActionPlanNodeEdge) bool { return edge.ProducerID == inputProducer }) ||
		!slices.ContainsFunc(node.Inputs, func(edge ActionPlanNodeEdge) bool { return edge.ProducerID == programProducer }) {
		t.Fatalf("relocs node=%#v recipe=%#v", node, recipe)
	}
}

func TestGenericKbuildSourceEvidenceRespectsProfileBinding(t *testing.T) {
	const source = "drivers/demo.c"
	withSource := mustCompactKbuildProfileForTest(t, "build:with-source", "scripts/Makefile.build", "", "", nil)
	withSource = compactKbuildProfileWithSourcesForTest(t, withSource, source)
	withoutSource := mustCompactKbuildProfileForTest(t, "build:without-source", "scripts/Makefile.build", "", "", nil)
	metadata := &CompactMetadata{Config: CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{withoutSource, withSource},
	}}

	unbound := newCompactKbuildRulePlanBuilder(metadata, &ActionPlan{})
	if exists, err := unbound.sourcePathExists(source); err != nil || exists {
		t.Fatalf("unbound builder borrowed %q from the global profile set", source)
	}
	if _, err := unbound.build("drivers/demo.o"); err == nil || !strings.Contains(err.Error(), "exact evaluated profile") {
		t.Fatalf("unbound rule lowering error=%v, want exact-profile requirement", err)
	}
	boundWithout := unbound.forProfile(withoutSource)
	if exists, err := boundWithout.sourcePathExists(source); err != nil || exists {
		t.Fatalf("builder bound to %q borrowed %q from another profile", withoutSource.Name, source)
	}
	boundWith := unbound.forProfile(withSource)
	if exists, err := boundWith.sourcePathExists(source); err != nil || !exists {
		t.Fatalf("builder bound to %q did not find its source %q", withSource.Name, source)
	}
}

func TestCompactKbuildProfileSourcePathUsesExactConfiguredRoots(t *testing.T) {
	kernelRoot := t.TempDir()
	rustRoot := t.TempDir()
	externalRoot := t.TempDir()
	objectRoot := t.TempDir()
	hostDepsRoot := t.TempDir()
	const (
		rustPrefix    = "external/rust-src/library"
		rustSource    = rustPrefix + "/core/src/lib.rs"
		externalPath  = "drivers/external/source.c"
		objectPath    = "include/generated/object.h"
		hostDepsPath  = "__LINUX_BZL_HOST_DEPS__/include/libelf.h"
		missingSource = rustPrefix + "/core/src/missing.rs"
	)
	mustWriteSource(t, kernelRoot, rustSource, "// wrong shadow\n")
	mustWriteSource(t, rustRoot, "core/src/lib.rs", "// selected Rust source\n")
	mustWriteSource(t, kernelRoot, externalPath, "// wrong shadow\n")
	mustWriteSource(t, externalRoot, "source.c", "// selected external source\n")
	mustWriteSource(t, objectRoot, objectPath, "// object tree\n")
	mustWriteSource(t, hostDepsRoot, "include/libelf.h", "// host deps\n")
	profile := mustCompactKbuildProfileForTest(t, "configured-roots", "scripts/Makefile.build", "", "", nil)
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":                  kernelRoot,
		"__LINUX_BZL_SOURCE_TREE__/drivers/external": externalRoot,
		"__LINUX_BZL_OBJECT_TREE__":                  objectRoot,
		"__LINUX_BZL_HOST_DEPS__":                    hostDepsRoot,
		rustPrefix:                                   rustRoot,
	}
	for _, test := range []struct {
		name   string
		path   string
		want   string
		exists bool
	}{
		{name: "direct Rust root", path: rustSource, want: filepath.Join(rustRoot, "core/src/lib.rs"), exists: true},
		{name: "missing direct Rust source", path: missingSource, want: filepath.Join(rustRoot, "core/src/missing.rs")},
		{name: "sentinel external overlay", path: externalPath, want: filepath.Join(externalRoot, "source.c"), exists: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ResolveCompactKbuildProfileSourcePath(profile, test.path)
			if !ok || filepath.Clean(got) != filepath.Clean(test.want) {
				t.Fatalf("ResolveCompactKbuildProfileSourcePath(%q) = %q, %t, want %q", test.path, got, ok, test.want)
			}
			if got := compactKbuildProfileSourcePathExists(profile, test.path); got != test.exists {
				t.Fatalf("compactKbuildProfileSourcePathExists(%q) = %t, want %t", test.path, got, test.exists)
			}
		})
	}
	for _, reserved := range []string{objectPath, hostDepsPath} {
		if compactKbuildProfileSourcePathExists(profile, reserved) {
			t.Fatalf("reserved root path %q was accepted as physical source evidence", reserved)
		}
	}
}

func TestPreconfiguredObjectTreeUsesOnlyExactExistingSourceLeaves(t *testing.T) {
	const (
		source  = "include/generated/sdk.h"
		missing = "include/generated/missing.h"
	)
	objectRoot := t.TempDir()
	mustWriteSource(t, objectRoot, source, "// sdk\n")
	profile := mustCompactKbuildProfileForTest(t, "external", "scripts/Makefile.build", "", "", nil)
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_OBJECT_TREE__": objectRoot}
	metadata := &CompactMetadata{
		preconfiguredObjectTree: true,
		exactSourceNamespaces: map[string]string{
			source:                    "prep",
			missing:                   "prep",
			"include/generated/broad": "prep",
		},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, &ActionPlan{}).forProfile(profile)
	if exists, err := builder.sourcePathExists(source); err != nil || !exists {
		t.Fatalf("preconfigured source exists = %t, %v, want true", exists, err)
	}
	for _, absent := range []string{missing, "include/generated/broad/not-present.h"} {
		if exists, err := builder.sourcePathExists(absent); err != nil || exists {
			t.Fatalf("preconfigured absent path %q exists = %t, %v, want false", absent, exists, err)
		}
	}
	plan := &ActionPlan{}
	id, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := plan.Sources, []ActionPlanSource{{ID: id, Namespace: "prep", Path: source}}; !slices.Equal(got, want) {
		t.Fatalf("preconfigured plan sources = %#v, want %#v", got, want)
	}
}

func TestExternalModpostKeepsKernelSymversInputDistinctFromModuleOutput(t *testing.T) {
	const (
		directory     = ".linux-bzl/external/demo"
		virtualRoot   = "__LINUX_BZL_SOURCE_TREE__/" + directory
		target        = directory + "/Module.symvers"
		kernelSymvers = "Module.symvers"
		modpost       = "scripts/mod/modpost"
	)
	objectRoot := t.TempDir()
	mustWriteSource(t, objectRoot, kernelSymvers, "kernel symbols\n")
	mustWriteSource(t, objectRoot, modpost, "prepared modpost\n")
	externalRoot := t.TempDir()
	mustWriteSource(t, externalRoot, "Kbuild", "obj-m += demo.o\n")
	profile := mustCompactKbuildProfileForTest(t, "external-modpost", "scripts/Makefile.modpost", directory, `
output-symdump := Module.symvers
MODPOST = $(objtree)/scripts/mod/modpost
modpost-args = -i $(objtree)/Module.symvers -o $@
modpost-deps := $(MODPOST)
modpost-deps += $(objtree)/Module.symvers
cmd_modpost = $(MODPOST) $(modpost-args)
$(output-symdump): $(modpost-deps) FORCE
	$(call if_changed,modpost)
FORCE:
`, map[string]string{"objtree": "__LINUX_BZL_OBJECT_TREE__"})
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":              t.TempDir(),
		"__LINUX_BZL_OBJECT_TREE__":              objectRoot,
		"__LINUX_BZL_SOURCE_TREE__/" + directory: externalRoot,
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		preconfiguredObjectTree: true,
		exactSourceNamespaces: map[string]string{
			kernelSymvers: "prep",
			modpost:       "prep",
		},
		sourceNamespaces: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__/" + directory: "external",
		},
		Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("target", "metadata", "module_symvers").
		withInitialObjectTree(true).
		forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("modpost producer %q is absent", producer)
	}
	if len(node.Outputs) != 1 || node.Outputs[0] != (ActionPlanOutput{Tree: "metadata", Path: target}) {
		t.Fatalf("modpost outputs = %#v, want metadata/%s", node.Outputs, target)
	}
	if len(node.Inputs) != 1 {
		t.Fatalf("modpost inputs = %#v, want one prepared program projection", node.Inputs)
	}
	recipe := plan.Recipes[node.Recipe]
	if !slices.Contains(sortedStringMapValues(recipe.WorkingInputs), kernelSymvers) {
		t.Fatalf("modpost working inputs = %#v, want preconfigured %s", recipe.WorkingInputs, kernelSymvers)
	}
	prepSource := ActionPlanSource{}
	for _, edge := range node.Sources {
		for _, source := range plan.Sources {
			if source.ID == edge.SourceID && source.Namespace == "prep" && source.Path == kernelSymvers {
				prepSource = source
			}
		}
	}
	if prepSource.ID == "" {
		t.Fatalf("modpost source edges = %#v from sources %#v, want prep/%s", node.Sources, plan.Sources, kernelSymvers)
	}
	if prepSource.Path != kernelSymvers || node.Outputs[0].Path != target || prepSource.Namespace == node.Outputs[0].Tree {
		t.Fatalf("modpost input %#v and output %#v do not retain distinct prep/metadata provenance", prepSource, node.Outputs[0])
	}
}

func TestExternalRootModpostStagesExactOverlaySymversForHermeticScript(t *testing.T) {
	const (
		directory     = ".linux-bzl/external/demo"
		virtualRoot   = "__LINUX_BZL_SOURCE_TREE__/" + directory
		target        = directory + "/Module.symvers"
		commandState  = directory + "/.hello_module.o.cmd"
		extraSymvers0 = directory + "/.linux-bzl-dependencies/00000000.symvers"
		extraSymvers1 = directory + "/.linux-bzl-dependencies/00000001.symvers"
		modpost       = "scripts/mod/modpost"
	)
	objectRoot := t.TempDir()
	mustWriteSource(t, objectRoot, modpost, "prepared modpost\n")
	externalRoot := t.TempDir()
	mustWriteSource(t, externalRoot, "hello_module.c", "int hello_module;\n")
	mustWriteSource(t, externalRoot, ".linux-bzl-dependencies/00000000.symvers", "0x1\tprovider\tprovider\tEXPORT_SYMBOL\n")
	mustWriteSource(t, externalRoot, ".linux-bzl-dependencies/00000001.symvers", "0x2\tsecond\tsecond\tEXPORT_SYMBOL\n")
	profile := mustCompactKbuildProfileForTest(t, "external-root-modpost", "scripts/Makefile.modpost", "", `
MODPOST = $(objtree)/scripts/mod/modpost
KBUILD_EXTRA_SYMBOLS := $(src)/.linux-bzl-dependencies/00000000.symvers $(src)/.linux-bzl-dependencies/00000001.symvers
modpost-args = -o $@ -e $(addprefix -i ,$(KBUILD_EXTRA_SYMBOLS)) `+commandState+`
cmd_modpost = if true; then $(MODPOST) $(modpost-args); fi
`+target+`: `+commandState+` $(MODPOST) FORCE
	$(call if_changed,modpost)
FORCE:
`, map[string]string{
		"objtree": "__LINUX_BZL_OBJECT_TREE__",
		"src":     virtualRoot,
	})
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":              t.TempDir(),
		"__LINUX_BZL_OBJECT_TREE__":              objectRoot,
		"__LINUX_BZL_SOURCE_TREE__/" + directory: externalRoot,
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		preconfiguredObjectTree: true,
		exactSourceNamespaces:   map[string]string{modpost: "prep"},
		sourceNamespaces: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__/" + directory: "external",
		},
		Config: CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	externalSourceID, err := metadata.ensureActionPlanSource(plan, directory+"/hello_module.c")
	if err != nil {
		t.Fatal(err)
	}
	commandStateProducer, err := appendActionPlanNode(plan, ActionPlanNode{
		Stage: "target", Kind: "generate", Tool: "actionfile", Product: "module",
		Sources: []ActionPlanSourceEdge{{Role: "source", SourceID: externalSourceID}},
		Trees:   []string{"external"},
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: commandState}},
	}, ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments:        []string{"-input", "${source:source:00000000}", "-out", "${output:00000000}"},
		Sources:          []string{"source:00000000"},
		Trees:            []string{"external"},
		WorkingTrees:     []string{"external"},
		WorkingDirectory: "external-compile",
		Outputs:          []string{"00000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("target", "metadata", "module_symvers").
		withInitialObjectTree(true).
		forProfile(profile)
	producer, err := builder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("root-level modpost producer %q is absent", producer)
	}
	if !slices.ContainsFunc(node.Inputs, func(edge ActionPlanNodeEdge) bool {
		return edge.ProducerID == commandStateProducer
	}) {
		t.Fatalf("root-level modpost inputs = %#v, want generated command metadata producer %q", node.Inputs, commandStateProducer)
	}
	recipe := plan.Recipes[node.Recipe]
	if recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("root-level modpost tool = %q, want compound %q action", recipe.Tool, compactKbuildScriptRunnerRole)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	workingInputs := sortedStringMapValues(recipe.WorkingInputs)
	for _, extraSymvers := range []string{extraSymvers0, extraSymvers1} {
		extraSource := ActionPlanSource{}
		for _, edge := range node.Sources {
			for _, source := range plan.Sources {
				if source.ID == edge.SourceID && source.Namespace == "external" && source.Path == extraSymvers {
					extraSource = source
				}
			}
		}
		if extraSource.ID == "" {
			t.Fatalf("root-level modpost sources = %#v from %#v, want external/%s in script %q", node.Sources, plan.Sources, extraSymvers, script)
		}
		if !slices.Contains(workingInputs, extraSymvers) {
			t.Fatalf("root-level modpost working inputs = %#v, want exact %s", recipe.WorkingInputs, extraSymvers)
		}
	}
	if !slices.Contains(node.Trees, "external") || !slices.Contains(recipe.Trees, "external") {
		t.Fatalf("root-level modpost trees = node %#v recipe %#v, want external namespace retained by %s", node.Trees, recipe.Trees, commandState)
	}
	if !slices.Contains(recipe.WorkingTrees, "external") {
		t.Fatalf("root-level modpost did not replay command-metadata working source namespace: %#v", recipe.WorkingTrees)
	}
}

func TestPreconfiguredObjectTreeLeafMakesImplicitRuleViable(t *testing.T) {
	const source = "vendor/driver.c"
	objectRoot := t.TempDir()
	mustWriteSource(t, objectRoot, source, "int driver;\n")
	profile := mustCompactKbuildProfileForTest(t, "external", "scripts/Makefile.build", "", `
cmd_copy = cp $< $@
%.o: %.c FORCE
	$(call if_changed,copy)
`, nil)
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_OBJECT_TREE__": objectRoot}
	metadata := &CompactMetadata{
		preconfiguredObjectTree: true,
		exactSourceNamespaces:   map[string]string{source: "prep"},
	}
	match, matched, err := metadata.compactKbuildRuleForProfile(profile, "vendor/driver.o")
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("preconfigured source did not select the implicit object-from-C rule")
	}
	viable, err := metadata.compactKbuildRuleMatchViableInProfile("vendor/driver.o", match, profile)
	if err != nil {
		t.Fatal(err)
	}
	if !viable {
		t.Fatal("implicit object-from-C rule rejected its exact preconfigured source leaf")
	}
	plan := &ActionPlan{}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	inputs, err := builder.ruleInputs("vendor/driver.o", match)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0].path != source || !inputs[0].objectTree || inputs[0].sourceID == "" {
		t.Fatalf("preconfigured implicit-rule inputs = %#v, want one prep/object-tree source leaf", inputs)
	}
	automatic, err := compactKbuildRuleRootedAutomaticEvaluationContext(
		"vendor/driver.o",
		match,
		inputs,
		map[string]string{"obj": "__LINUX_BZL_OBJECT_TREE__", "src": "__LINUX_BZL_SOURCE_TREE__"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(automatic.normal, "__LINUX_BZL_OBJECT_TREE__/"+source) {
		t.Fatalf("rooted automatic prerequisites = %q, want object-tree source %q", automatic.normal, source)
	}
}

func TestGenericKbuildExistingProducerFindsNativeProductTrees(t *testing.T) {
	const (
		moduleTarget = "drivers/demo.ko"
		imageTarget  = "arch/x86/boot/bzImage"
	)
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{ID: strings.Repeat("a", 64), Stage: "target", Outputs: []ActionPlanOutput{{Tree: "modules", Path: moduleTarget}}},
		{ID: strings.Repeat("b", 64), Stage: "target", Outputs: []ActionPlanOutput{{Tree: "image", Path: imageTarget}}},
	}}
	builder := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan)
	for target, want := range map[string]string{
		moduleTarget: strings.Repeat("a", 64),
		imageTarget:  strings.Repeat("b", 64),
	} {
		producer, slot, ok := builder.existingProducer(target)
		if !ok || producer != want || slot != 0 {
			t.Errorf("existingProducer(%q) = (%q, %d, %t), want (%q, 0, true)", target, producer, slot, ok, want)
		}
	}
}

func TestGenericKbuildExistingProducerRespectsConsumerStage(t *testing.T) {
	const target = "include/generated/example.h"
	targetProducer := strings.Repeat("a", 64)
	prepProducer := strings.Repeat("b", 64)
	bootstrapProducer := strings.Repeat("c", 64)
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{ID: targetProducer, Stage: "target", Outputs: []ActionPlanOutput{{Tree: "objects", Path: target}}},
		{ID: prepProducer, Stage: "prep", Outputs: []ActionPlanOutput{{Tree: "prep", Path: target}}},
		{ID: bootstrapProducer, Stage: "bootstrap", Outputs: []ActionPlanOutput{{Tree: "bootstrap", Path: target}}},
	}}
	host := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).forOutput("host", "host", "sdk")
	if got, _, ok := host.existingProducer(target); !ok || got != bootstrapProducer {
		t.Fatalf("host existing producer=(%q,%t), want bootstrap producer %q", got, ok, bootstrapProducer)
	}
	prep := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).forOutput("prep", "prep", "sdk")
	if got, _, ok := prep.existingProducer(target); !ok || got != prepProducer {
		t.Fatalf("prep existing producer=(%q,%t), want prep producer %q", got, ok, prepProducer)
	}

	prepOnly := &ActionPlan{Nodes: []ActionPlanNode{{
		ID: prepProducer, Stage: "prep", Outputs: []ActionPlanOutput{{Tree: "prep", Path: target}},
	}}}
	host = newCompactKbuildRulePlanBuilder(&CompactMetadata{}, prepOnly).forOutput("host", "host", "sdk")
	if got, slot, ok := host.existingProducer(target); ok {
		t.Fatalf("host accepted later prep producer (%q,%d,%t)", got, slot, ok)
	}
}

func TestGenericKbuildExistingInputRebasesLaterProjectionProvenance(t *testing.T) {
	const (
		nativePath  = "generated/native.h"
		logicalPath = "include/generated/logical.h"
		configPath  = "include/generated/autoconf.h"
	)
	bootstrapID := strings.Repeat("a", 64)
	inputProjectionID := strings.Repeat("b", 64)
	sourceProjectionID := strings.Repeat("c", 64)
	sourceID := "src-00000001"
	plan := &ActionPlan{Nodes: []ActionPlanNode{
		{ID: bootstrapID, Stage: "bootstrap", Kind: "generate", Tool: "cc", Outputs: []ActionPlanOutput{{Tree: "bootstrap", Path: nativePath}}},
		{
			ID: inputProjectionID, Stage: "prep", Kind: "copy", Tool: "actionfile",
			Inputs:  []ActionPlanNodeEdge{{Role: "input", ProducerID: bootstrapID, Slot: 0}},
			Outputs: []ActionPlanOutput{{Tree: "prep", Path: logicalPath}},
		},
		{
			ID: sourceProjectionID, Stage: "prep", Kind: "copy", Tool: "actionfile",
			Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: sourceID}},
			Outputs: []ActionPlanOutput{{Tree: "prep", Path: configPath}},
		},
	}}
	host := newCompactKbuildRulePlanBuilder(&CompactMetadata{}, plan).forOutput("host", "host", "sdk")
	input, ok, err := host.existingInput(logicalPath)
	if err != nil || !ok || input.path != logicalPath || input.producer != bootstrapID || input.slot != 0 || input.sourceID != "" {
		t.Fatalf("input-backed projection=(%#v,%t,%v), want bootstrap provenance at logical path", input, ok, err)
	}
	input, ok, err = host.existingInput(configPath)
	if err != nil || !ok || input.path != configPath || input.sourceID != sourceID || input.producer != "" {
		t.Fatalf("source-backed projection=(%#v,%t,%v), want config source provenance at logical path", input, ok, err)
	}
}

func TestGenericKbuildRuleInputsKeepObjectRootedPrerequisiteOutsideProfileCwd(t *testing.T) {
	const (
		target       = "tools/lib/subcmd/install_headers"
		prerequisite = "tools/objtool/libsubcmd/include/subcmd/exec-cmd.h"
	)
	profile := mustCompactKbuildProfileForTest(
		t,
		"driver:tools/lib/subcmd/Makefile",
		"tools/lib/subcmd/Makefile",
		"tools/lib/subcmd",
		`cmd_install = cp $< $@
install_headers: $(objtree)/tools/objtool/libsubcmd/include/subcmd/exec-cmd.h
	$(call if_changed,install)
`,
		map[string]string{"objtree": "__LINUX_BZL_OBJECT_TREE__"},
	)
	// Model an external-module SDK that already contains this path while the
	// module's prep graph deliberately replaces it with an exact producer.
	profile = compactKbuildProfileWithSourcesForTest(t, profile, prerequisite)
	metadata := &CompactMetadata{Config: CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
	}}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	wantProducer := appendSelectionScopeTestOutput(t, plan, "prep", "prep", prerequisite)
	match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("missing evaluated rule for %q", target)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	inputs, err := builder.ruleInputs(target, match)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(inputs), 1; got != want || inputs[0].path != prerequisite {
		t.Fatalf("canonical rule inputs = %#v, want prerequisite %q", inputs, prerequisite)
	}
	if inputs[0].producer != wantProducer || inputs[0].sourceID != "" {
		t.Fatalf("same-path input = %#v, want exact prep producer %q instead of source evidence", inputs[0], wantProducer)
	}
}

func TestUnmatchedLinkerScriptIsNotSynthesized(t *testing.T) {
	const target = "arch/example/vmlinux.lds"
	profile := mustCompactKbuildProfileForTest(t, "build:arch/example", "scripts/Makefile.build", "arch/example", `
cmd_cpp_lds_S = $(CC) $(CPPFLAGS_$(target-stem).lds) -E -P -o $@ $<
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"), "CPPFLAGS_vmlinux.lds": "-DEXACT_TARGET_FLAGS",
	})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, target+".S")
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, &ActionPlan{Recipes: map[string]ActionRecipe{}}).forProfile(profile)
	_, err := builder.build(target)
	if err == nil || !strings.Contains(err.Error(), "no evaluated Kbuild rule") {
		t.Fatalf("unmatched linker-script error=%v, want exact-rule failure", err)
	}
}

func TestGenericKbuildRecipeLowersDirectFilechkAndErasesShellMkdir(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "prep:featuremasks", "arch/x86/Makefile", "", `
AWK = /selected/awk
filechk_gen_featuremasks = $(AWK) -f arch/x86/tools/cpufeaturemasks.awk arch/x86/include/asm/cpufeatures.h .config
arch/x86/include/generated/asm/cpufeaturemasks.h: arch/x86/tools/cpufeaturemasks.awk arch/x86/include/asm/cpufeatures.h .config FORCE
	$(shell mkdir -p $(dir $@))
	$(call filechk,gen_featuremasks)
`, map[string]string{"AWK": KbuildActionRoleToken("target", "awk")})
	target := "arch/x86/include/generated/asm/cpufeaturemasks.h"
	profile = compactKbuildProfileWithSourcesForTest(
		t, profile,
		"arch/x86/tools/cpufeaturemasks.awk",
		"arch/x86/include/asm/cpufeatures.h",
	)
	script, ok := ResolveCompactKbuildProfileSourcePath(profile, "arch/x86/tools/cpufeaturemasks.awk")
	if !ok {
		t.Fatal("cannot resolve cpufeaturemasks AWK source")
	}
	if err := os.WriteFile(script, []byte(projectedGeneratorTestAWK), 0o644); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}, Nodes: []ActionPlanNode{{
		ID: strings.Repeat("c", 64), Stage: "prep", Kind: "copy", Tool: "actionfile", Product: "sdk",
		Outputs: []ActionPlanOutput{{Tree: "prep", Path: ".config"}},
	}}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 2; got != want {
		t.Fatalf("node count=%d, want %d: %#v", got, want, plan.Nodes)
	}
	result := plan.Nodes[1]
	recipe := plan.Recipes[result.Recipe]
	if result.Outputs[0].Path != target || result.Tool != "awk" || recipe.Tool != "awk" || recipe.Stdout != "00000000" {
		t.Fatalf("filechk result=%#v recipe=%#v", result, recipe)
	}
	if len(recipe.ExecutableInputs) != 0 || strings.HasPrefix(recipe.Tool, "input:") {
		t.Fatalf("filechk AWK is not identity-bound: %#v", recipe)
	}
	if got, want := plan.projectedGeneratorCandidates[result.ID], []string{
		"CONFIG_X86_DISABLED_FEATURE_",
		"CONFIG_X86_REQUIRED_FEATURE_",
	}; got.TargetPath != target || got.TargetSlot != 0 || !slices.Equal(got.ConfigProjectionPrefixes, want) {
		t.Fatalf("filechk projected Kconfig candidate=%#v, want %#v", got, want)
	}
}

func TestGenericKbuildDirectRecipeErasesShellDirectorySetup(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "host:generated", "scripts/host.mk", "", `
generated-tool: input.c FORCE
	$(shell mkdir -p $(dir $@))
	$(HOSTCC) -o $@ $<
`, map[string]string{"HOSTCC": KbuildActionRoleToken("host", "cc")})
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.c")
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forProfile(profile).
		forOutput("host", "host", "sdk")
	if _, err := builder.build("generated-tool"); err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 || plan.Nodes[0].Stage != "host" {
		t.Fatalf("direct generated-tool nodes = %#v, want one host action", plan.Nodes)
	}
}

func TestGenericKbuildDirectRecipeKeepsPipelineAtomic(t *testing.T) {
	const target = "generated/result.txt"
	profile := mustCompactKbuildProfileForTest(t, "target:generated", "Kbuild", "", `
generated/result.txt: input.txt FORCE
	printf '%s' $< | sed 's/input/result/' > $@
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.txt")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("direct pipeline node count=%d, want one compound action: %#v", got, plan.Nodes)
	}
	node := plan.Nodes[0]
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Stdin != "" || recipe.Stdout != "" ||
		recipe.WorkingOutputs["00000000"] != target {
		t.Fatalf("direct atomic pipeline node=%#v recipe=%#v", node, recipe)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if !strings.Contains(script, `printf '%s' ${tree:kernel}/input.txt | sed 's/input/result/' > `+target) ||
		strings.Contains(script, ".linux-bzl-intermediate") {
		t.Fatalf("direct atomic pipeline script=%q", script)
	}
}

func TestGenericKbuildDirectFilechkKeepsParseErrorPipelineAtomic(t *testing.T) {
	const target = "generated/result.txt"
	profile := mustCompactKbuildProfileForTest(t, "target:generated", "Kbuild", "", `
filechk_result = tools/filter input.txt | sed 's/input/result/' || true
generated/result.txt: tools/filter input.txt FORCE
	$(call filechk,result)
`, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.txt")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := compactGenericRecipePlanForTest()
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 2; got != want {
		t.Fatalf("filechk compound node count=%d, want tool + action: %#v", got, plan.Nodes)
	}
	node := plan.Nodes[1]
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || len(recipe.ExecutableInputs) != 1 || recipe.WorkingOutputs["00000000"] != target {
		t.Fatalf("filechk parse-error compound node=%#v recipe=%#v", node, recipe)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	if !strings.Contains(script, "{\ntools/filter input.txt | sed 's/input/result/' || true\n} > "+target) {
		t.Fatalf("filechk parse-error compound script=%q", script)
	}
}

func TestGenericKbuildCompoundRequiresTypedInvocationLocation(t *testing.T) {
	metadata, target := compactGenericRecipeMetadataForTest(
		t, `printf '%s' payload | sed 's/payload/result/' > $@`, ":", "generated/result.h",
	)
	profile := metadata.Config.KbuildProfiles[0]
	profile.invocationLocation = CompactKbuildInvocationLocation{}
	profile.invocationLocationSet = false
	metadata.Config.KbuildProfiles[0] = profile
	err := buildCompactKbuildTargetForTest(metadata, compactGenericRecipePlanForTest(), target)
	if err == nil || !strings.Contains(err.Error(), "typed Kbuild execution requires an invocation location") {
		t.Fatalf("untyped compound error=%v", err)
	}
}

func TestGenericKbuildCompoundPreservesSplitSourceAndObjectRoots(t *testing.T) {
	const (
		directory = "tools/objtool/libsubcmd"
		target    = directory + "/libsubcmd-in.o"
	)
	objects := []string{
		directory + "/exec-cmd.o",
		directory + "/help.o",
	}
	profile := mustCompactKbuildProfileForTest(t, "build:libsubcmd", "tools/build/Makefile.build", "libsubcmd", `
obj-y := $(addprefix $(OUTPUT),exec-cmd.o help.o)
cmd_mkdir = mkdir -p $(dir $@)
cmd_ld_multi = $(LD) -r -o $@ $(filter $(obj-y),$^)
$(OUTPUT)libsubcmd-in.o: $(obj-y) FORCE
	$(cmd_mkdir)
	$(cmd_ld_multi)
`, map[string]string{
		"LD":     KbuildActionRoleToken("host", "ld"),
		"OUTPUT": "__LINUX_BZL_OBJECT_TREE__/" + directory + "/",
	})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: "tools/lib/subcmd",
	}); err != nil {
		t.Fatal(err)
	}
	if got, ok := CompactKbuildProfileInvocationLocation(profile); !ok || got != (CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: "tools/lib/subcmd",
	}) {
		t.Fatalf("split-root Make process location = (%#v, %t)", got, ok)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	inputs := make([]compactKbuildRuleInput, 0, len(objects))
	for _, object := range objects {
		node := ActionPlanNode{
			Stage: "host", Kind: "compile", Tool: "cc", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: "host", Path: object}},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
			Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
		}
		producer, err := appendActionPlanNode(plan, node, recipe)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, compactKbuildRuleInput{path: object, producer: producer})
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("host", "host", "sdk").
		forProfile(profile)
	rootedDirectory := compactKbuildActionObjectTreeMarker + "/" + directory
	template := "mkdir -p " + rootedDirectory + "/; " + KbuildActionRoleToken("host", "ld") +
		" -r -o " + rootedDirectory + "/libsubcmd-in.o " +
		rootedDirectory + "/exec-cmd.o " + rootedDirectory + "/help.o"
	commands, err := compactKbuildCompoundProgramCommands(template)
	if err != nil {
		t.Fatal(err)
	}
	producer, err := builder.buildHermeticKbuildCompound(target, compactKbuildRuleMatch{
		profile: profile,
		rule:    profile.Rules[0],
	}, inputs, template, commands)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("libsubcmd aggregate producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("split-root aggregate tool = %q, want atomic %q", node.Tool, compactKbuildScriptRunnerRole)
	}
	if got, want := recipe.ExecutionDirectory, "tools/lib/subcmd"; got != want {
		t.Fatalf("split-root execution directory = %q, want %q", got, want)
	}
	if got := recipe.WorkingOutputs["00000000"]; got != target {
		t.Fatalf("split-root working output = %q, want %q", got, target)
	}
	script := compactKbuildRecipeScriptContentForTest(t, recipe)
	for _, want := range []string{
		"mkdir -p ../../../tools/objtool/libsubcmd/",
		"-o ../../../tools/objtool/libsubcmd/libsubcmd-in.o",
		"../../../tools/objtool/libsubcmd/exec-cmd.o",
		"../../../tools/objtool/libsubcmd/help.o",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("split-root compound script %q omits %q", script, want)
		}
	}
}

func TestGenericKbuildSplitRootSavecmdSideOutputStaysObjectRooted(t *testing.T) {
	const (
		directory   = "tools/objtool/libsubcmd"
		target      = directory + "/libsubcmd-in.o"
		commandFile = directory + "/.libsubcmd-in.o.cmd"
	)
	objects := []string{
		directory + "/exec-cmd.o",
		directory + "/help.o",
	}
	sourceRoot := filepath.ToSlash(t.TempDir())
	objectRoot := filepath.ToSlash(t.TempDir())
	variables := map[string]string{
		"LD":     KbuildActionRoleToken("host", "ld"),
		"OUTPUT": objectRoot + "/" + directory + "/",
	}
	kb, err := parseKbuildWithOptions(strings.NewReader(`
obj-y := $(addprefix $(OUTPUT),exec-cmd.o help.o)
cmd = $(cmd_$(1))
make-cmd = $(cmd_$(1))
dot-target = $(dir $@).$(notdir $@)
cmd_and_savecmd = $(cmd); printf '%s\n' 'savedcmd_$@ := $(make-cmd)' > $(dot-target).cmd
if_changed = $(cmd_and_savecmd)
cmd_ld_multi = $(LD) -r -o $@ $(filter $(obj-y),$^) && test -f $@
$(OUTPUT)libsubcmd-in.o: $(obj-y) FORCE
	$(call if_changed,ld_multi)
	`), "tools/build/Makefile.build", KbuildOptions{
		SourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": sourceRoot,
			"__LINUX_BZL_OBJECT_TREE__": objectRoot,
		},
		Variables:               variables,
		CommandLineVariables:    map[string]string{"LD": variables["LD"]},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("build:libsubcmd", "tools/build/Makefile.build", "", kb)
	if err != nil {
		t.Fatal(err)
	}
	profile.Directory = "libsubcmd"
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: "tools/lib/subcmd",
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("split-root savecmd target %q did not match", target)
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	inputs := make([]compactKbuildRuleInput, 0, len(objects))
	for _, object := range objects {
		node := ActionPlanNode{
			Stage: "host", Kind: "compile", Tool: "cc", Product: "sdk",
			Outputs: []ActionPlanOutput{{Tree: "host", Path: object}},
		}
		recipe := ActionRecipe{
			Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
			Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
		}
		producer, appendErr := appendActionPlanNode(plan, node, recipe)
		if appendErr != nil {
			t.Fatal(appendErr)
		}
		inputs = append(inputs, compactKbuildRuleInput{path: object, producer: producer})
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("host", "host", "sdk").
		forProfile(profile)
	producer, err := builder.buildCommandTemplate(target, match, inputs)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("split-root savecmd producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("split-root savecmd node = %#v recipe = %#v, want scriptrun", node, recipe)
	}
	foundCommandFile := false
	for _, output := range recipe.WorkingOutputs {
		foundCommandFile = foundCommandFile || output == commandFile
		if strings.Contains(output, "tools/lib/subcmd/tools/objtool") {
			t.Fatalf("split-root side output was scoped below source cwd: %#v", recipe.WorkingOutputs)
		}
	}
	if !foundCommandFile {
		t.Fatalf("split-root savecmd outputs = %#v, want %q", recipe.WorkingOutputs, commandFile)
	}
}

func TestGenericKbuildSplitRootStubcopySavecmdSideOutputStaysObjectRooted(t *testing.T) {
	const (
		directory   = "drivers/firmware/efi/libstub"
		target      = directory + "/alignedmem.stub.o"
		input       = directory + "/alignedmem.o"
		commandFile = directory + "/.alignedmem.stub.o.cmd"
	)
	sourceRoot := filepath.ToSlash(t.TempDir())
	objectRoot := filepath.ToSlash(t.TempDir())
	variables := map[string]string{
		"STRIP":   KbuildActionRoleToken("target", "strip"),
		"OBJDUMP": KbuildActionRoleToken("target", "objdump"),
		"OBJCOPY": KbuildActionRoleToken("target", "objcopy"),
		"OUTPUT":  objectRoot + "/" + directory + "/",
	}
	kb, err := parseKbuildWithOptions(strings.NewReader(`
cmd = $(cmd_$(1))
make-cmd = $(cmd_$(1))
dot-target = $(dir $@).$(notdir $@)
cmd_and_savecmd = $(cmd); printf '%s\n' 'savedcmd_$@ := $(make-cmd)' > $(dot-target).cmd
if_changed = $(cmd_and_savecmd)
cmd_stubcopy = $(STRIP) --strip-debug -o $@ $<; if $(OBJDUMP) -r $@ | grep R_AARCH64_ABS; then echo "$@: absolute symbol references not allowed in the EFI stub" >&2; /bin/false; fi; $(OBJCOPY) --remove-section=.note.gnu.property --prefix-alloc-sections=.init --prefix-symbols=__efistub_ $< $@
$(OUTPUT)alignedmem.stub.o: $(OUTPUT)alignedmem.o FORCE
	$(call if_changed,stubcopy)
	`), "drivers/firmware/efi/libstub/Makefile", KbuildOptions{
		SourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": sourceRoot,
			"__LINUX_BZL_OBJECT_TREE__": objectRoot,
		},
		Variables: variables,
		CommandLineVariables: map[string]string{
			"STRIP": variables["STRIP"], "OBJDUMP": variables["OBJDUMP"], "OBJCOPY": variables["OBJCOPY"],
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("build:libstub", "drivers/firmware/efi/libstub/Makefile", "", kb)
	if err != nil {
		t.Fatal(err)
	}
	profile.Directory = "libstub"
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	match, found, err := metadata.compactKbuildRuleForProfile(profile, target)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("split-root stubcopy target %q did not match", target)
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
		Products: []ActionPlanProduct{{Name: "vmlinux", Tree: "objects", Path: target}},
	}
	inputNode := ActionPlanNode{
		Stage: "target", Kind: "compile", Tool: "cc", Product: "vmlinux",
		Outputs: []ActionPlanOutput{{Tree: "objects", Path: input}},
	}
	inputRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "compile", Tool: "cc",
		Arguments: []string{"-o", "${output:00000000}"}, Outputs: []string{"00000000"},
	}
	inputProducer, err := appendActionPlanNode(plan, inputNode, inputRecipe)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		forOutput("target", "objects", "vmlinux").
		forProfile(profile)
	producer, err := builder.buildCommandTemplate(
		target, match, []compactKbuildRuleInput{{path: input, producer: inputProducer}},
	)
	if err != nil {
		t.Fatal(err)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("split-root stubcopy producer %q not found", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != compactKbuildScriptRunnerRole || recipe.Tool != compactKbuildScriptRunnerRole {
		t.Fatalf("split-root stubcopy node = %#v recipe = %#v, want scriptrun", node, recipe)
	}
	foundCommandFile := false
	for _, output := range recipe.WorkingOutputs {
		foundCommandFile = foundCommandFile || output == commandFile
		if strings.Contains(output, directory+"/"+directory) {
			t.Fatalf("split-root stubcopy side output was scoped below source cwd: %#v", recipe.WorkingOutputs)
		}
	}
	if !foundCommandFile {
		t.Fatalf("split-root stubcopy outputs = %#v, want %q", recipe.WorkingOutputs, commandFile)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("split-root stubcopy action plan is invalid: %v", err)
	}
}

func TestGenericKbuildRecipeCanonicalizesAbsoluteSourceArgument(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "arch", "x86", "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "arch", "x86", "tools", "feature.awk"), []byte("{ print }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWriteSource(t, root, "arch/x86/include/asm/features.h", "#define FEATURE 1\n")
	makefile := fmt.Sprintf(`
AWK = /selected/awk
filechk_feature = $(AWK) -f $(srctree)/arch/x86/tools/feature.awk arch/x86/include/asm/features.h
arch/x86/include/generated/asm/feature.h: %s/arch/x86/tools/feature.awk arch/x86/include/asm/features.h FORCE
	$(call filechk,feature)
`, filepath.ToSlash(root))
	kb, err := parseKbuildWithOptions(strings.NewReader(makefile), "Makefile", KbuildOptions{
		Variables: map[string]string{"srctree": filepath.ToSlash(root)},
		CommandLineVariables: map[string]string{
			"AWK": KbuildActionRoleToken("target", "awk"),
		},
		SourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": root,
			"__LINUX_BZL_OBJECT_TREE__": root,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("prep:feature", "Makefile", "", kb)
	if err != nil {
		t.Fatal(err)
	}
	target := "arch/x86/include/generated/asm/feature.h"
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("node count=%d, want %d: %#v", got, want, plan.Nodes)
	}
	for _, source := range plan.Sources {
		if filepath.IsAbs(source.Path) || strings.Contains(source.Path, filepath.ToSlash(root)) {
			t.Fatalf("action plan retained executor-specific source path: %#v", source)
		}
	}
	recipe := plan.Recipes[plan.Nodes[0].Recipe]
	for _, argument := range recipe.Arguments {
		if strings.Contains(argument, filepath.ToSlash(root)) {
			t.Fatalf("action recipe retained executor-specific source root in %q: %#v", argument, recipe)
		}
	}
}

func TestGenericKbuildRecipeLowersLiteralFilechkWithConstantBranch(t *testing.T) {
	makefile := `
define filechk_version_header
	if [ 2 -gt 255 ]; then \
		echo \#define LINUX_VERSION_CODE 397055; \
	else \
		echo \#define LINUX_VERSION_CODE 397826; \
	fi; \
		echo    '#define KERNEL_VERSION(a,b,c) (((a) << 16) + ((b) << 8) + ` + "\t\t" + `((c) > 255 ? 255 : (c)))'; \
	echo   \#define    LINUX_VERSION_MAJOR   6
endef
include/generated/uapi/linux/version.h: FORCE
	$(call filechk,version_header)
`
	profile := mustCompactKbuildProfileForTest(t, "prep:version", "Makefile", "", makefile, nil)
	target := "include/generated/uapi/linux/version.h"
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("node count=%d, want %d: %#v", got, want, plan.Nodes)
	}
	node := plan.Nodes[0]
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != "actionfile" || recipe.Tool != "actionfile" || node.Outputs[0].Path != target {
		t.Fatalf("literal filechk node=%#v recipe=%#v", node, recipe)
	}
	wantLines := []string{
		"#define LINUX_VERSION_CODE 397826",
		"#define KERNEL_VERSION(a,b,c) (((a) << 16) + ((b) << 8) + \t\t((c) > 255 ? 255 : (c)))",
		"#define LINUX_VERSION_MAJOR 6",
	}
	gotLines := []string{}
	for index := 0; index+1 < len(recipe.Arguments); index++ {
		if recipe.Arguments[index] == "-line" {
			gotLines = append(gotLines, recipe.Arguments[index+1])
		}
	}
	if !slices.Equal(gotLines, wantLines) {
		t.Fatalf("literal filechk lines=%q, want %q; recipe=%#v", gotLines, wantLines, recipe)
	}
	for _, line := range gotLines {
		if strings.ContainsAny(line, "\r\n") {
			t.Fatalf("literal filechk emitted a multi-line argument %q", line)
		}
	}

	observedPlan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	observedBuilder := newCompactKbuildRulePlanBuilder(metadata, observedPlan).forProfile(profile)
	observedBuilder, err := observedBuilder.forObservedOutputs(target, []compactKbuildObservedOutput{{
		output: ActionPlanOutput{Tree: "metadata", Path: ".captures/literal-version-header"},
		path:   "generated/opaque.version",
	}})
	if err != nil {
		t.Fatal(err)
	}
	observedProducer, err := observedBuilder.build(target)
	if err != nil {
		t.Fatal(err)
	}
	observedNode, ok := compactKbuildPlanNode(observedPlan, observedProducer)
	if !ok {
		t.Fatalf("missing observed literal filechk producer %q", observedProducer)
	}
	observedRecipe := observedPlan.Recipes[observedNode.Recipe]
	if got, want := len(observedNode.Outputs), 2; got != want || observedNode.Outputs[1].ObservedPath != "generated/opaque.version" {
		t.Fatalf("observed literal filechk outputs = %#v, want target plus capture", observedNode.Outputs)
	}
	if observedRecipe.WorkingDirectory == "" || observedRecipe.ObservedOutputs["00000001"] != "generated/opaque.version" || observedRecipe.WorkingOutputs["00000001"] != "" {
		t.Fatalf("observed literal filechk recipe = %#v, want private cwd and runtime capture", observedRecipe)
	}
	if _, err := observedPlan.entries(); err != nil {
		t.Fatalf("observed literal filechk plan is invalid: %v", err)
	}
}

func TestGenericKbuildRecipePassesFilechkCallArguments(t *testing.T) {
	makefile := `
define filechk_offsets
	echo "#ifndef $2"; \
	echo "#define $2"; \
	echo "#endif"
endef
generated.h: FORCE
	$(call filechk,offsets,__GENERATED_OFFSETS_H__)
`
	profile := mustCompactKbuildProfileForTest(t, "prep:offsets", "Makefile", "", makefile, nil)
	target := "generated.h"
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	recipe := plan.Recipes[plan.Nodes[0].Recipe]
	wantLines := []string{"#ifndef __GENERATED_OFFSETS_H__", "#define __GENERATED_OFFSETS_H__", "#endif"}
	gotLines := []string{}
	for index := 0; index+1 < len(recipe.Arguments); index++ {
		if recipe.Arguments[index] == "-line" {
			gotLines = append(gotLines, recipe.Arguments[index+1])
		}
	}
	if !slices.Equal(gotLines, wantLines) {
		t.Fatalf("filechk argument lines = %q, want %q; recipe=%#v", gotLines, wantLines, recipe)
	}
}

func TestGenericKbuildOffsetsFilechkCarriesValidatedMacroHeaderContract(t *testing.T) {
	makefile := `
define sed-offsets
's:^[[:space:]]*\.ascii[[:space:]]*"\(.*\)".*:\1:; \
/^->/{s:->#\(.*\):/* \1 */:; \
s:^->\([^ ]*\) [\$$#]*\([^ ]*\) \(.*\):#define \1 \2 /* \3 */:; \
s:->::; p;}'
endef
define filechk_offsets
	echo "#ifndef $2"; \
	echo "#define $2"; \
	echo "/* generated */"; \
	sed -ne $(sed-offsets) < $<; \
	echo "#endif"
endef
generated.h: offsets.s FORCE
	$(call filechk,offsets,__GENERATED_OFFSETS_H__)
`
	profile := mustCompactKbuildProfileForTest(t, "prep:validated-offsets", "Makefile", "", makefile, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "offsets.s")
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, "generated.h"); err != nil {
		t.Fatal(err)
	}
	producer, _, ok := planProducerByOutput(plan, "objects", "generated.h")
	if !ok {
		t.Fatalf("validated offsets plan has no generated.h producer: %#v", plan.Nodes)
	}
	node, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("validated offsets producer %q is absent", producer)
	}
	recipe := plan.Recipes[node.Recipe]
	if recipe.Tool != "actionfile" || !slices.Contains(recipe.Arguments, "-validate_config_independent_macro_header_v1") {
		t.Fatalf("validated offsets final recipe = %#v, want actionfile validation contract", recipe)
	}
	if len(recipe.Environment) != 0 {
		t.Fatalf("validated offsets final actionfile retained source-exported environment: %#v", recipe.Environment)
	}

	makefile = strings.Replace(makefile, "s:->::; p;", "s:->::; s:SAFE:UNSAFE:; p;", 1)
	profile = mustCompactKbuildProfileForTest(t, "prep:unrecognized-offsets", "Makefile", "", makefile, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "offsets.s")
	metadata.Config.KbuildProfiles = []CompactKbuildProfile{profile}
	plan = &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, "generated.h"); err != nil {
		t.Fatal(err)
	}
	producer, _, ok = planProducerByOutput(plan, "objects", "generated.h")
	if !ok {
		t.Fatalf("unrecognized offsets plan has no generated.h producer: %#v", plan.Nodes)
	}
	node, _ = compactKbuildPlanNode(plan, producer)
	if recipe = plan.Recipes[node.Recipe]; slices.Contains(recipe.Arguments, "-validate_config_independent_macro_header_v1") {
		t.Fatalf("modified offsets helper incorrectly received validation contract: %#v", recipe)
	}
}

func TestGenericKbuildRecipeConcatenatesMultiCommandFilechk(t *testing.T) {
	makefile := `
filechk_transform = echo begin; sed -ne 's:^value$:VALUE:p' < $<; echo end
generated.h: input.txt FORCE
	$(call filechk,transform)
`
	profile := mustCompactKbuildProfileForTest(t, "prep:transform", "Makefile", "", makefile, nil)
	profile = compactKbuildProfileWithSourcesForTest(t, profile, "input.txt")
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, "generated.h"); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 4; got != want {
		t.Fatalf("multi-command filechk node count = %d, want %d: %#v", got, want, plan.Nodes)
	}
	producer, _, ok := planProducerByOutput(plan, "objects", "generated.h")
	if !ok {
		t.Fatalf("multi-command filechk has no generated.h producer: %#v", plan.Nodes)
	}
	var final ActionPlanNode
	for _, node := range plan.Nodes {
		if node.ID == producer {
			final = node
			break
		}
	}
	recipe := plan.Recipes[final.Recipe]
	if final.Tool != "actionfile" || recipe.Tool != "actionfile" {
		t.Fatalf("multi-command filechk final node=%#v recipe=%#v", final, recipe)
	}
	inputCount := 0
	for _, argument := range recipe.Arguments {
		if argument == "-input" {
			inputCount++
		}
	}
	if inputCount != 3 {
		t.Fatalf("multi-command filechk inputs = %d, want 3: %#v", inputCount, recipe)
	}

	observedPlan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	observedBuilder := newCompactKbuildRulePlanBuilder(metadata, observedPlan).forProfile(profile)
	var err error
	observedBuilder, err = observedBuilder.forObservedOutputs("generated.h", []compactKbuildObservedOutput{{
		output: ActionPlanOutput{Tree: "metadata", Path: ".captures/multi-filechk"},
		path:   "generated/opaque.filechk",
	}})
	if err != nil {
		t.Fatal(err)
	}
	observedProducer, err := observedBuilder.build("generated.h")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(observedPlan.Nodes), 4; got != want {
		t.Fatalf("observed multi-command filechk node count = %d, want split command sequence: %#v", got, observedPlan.Nodes)
	}
	previousProducer, previousStateSlot := "", -1
	for commandIndex, observedNode := range observedPlan.Nodes {
		observedRecipe := observedPlan.Recipes[observedNode.Recipe]
		stateSlots := []int{}
		for slot, output := range observedNode.Outputs {
			if output.ObservedPath == "generated/opaque.filechk" {
				stateSlots = append(stateSlots, slot)
			}
		}
		if len(stateSlots) != 1 {
			t.Fatalf("observed command %d state outputs = %#v, want one opaque state", commandIndex, observedNode.Outputs)
		}
		stateSlot := stateSlots[0]
		stateOutput := observedNode.Outputs[stateSlot]
		stateBinding := planOrdinal(stateSlot)
		if observedRecipe.ObservedOutputs[stateBinding] != "generated/opaque.filechk" {
			t.Fatalf("observed command %d recipe = %#v, want state binding %s", commandIndex, observedRecipe, stateBinding)
		}
		if commandIndex+1 == len(observedPlan.Nodes) {
			if observedNode.ID != observedProducer || stateOutput.Path != ".captures/multi-filechk" || observedNode.Tool != "actionfile" {
				t.Fatalf("final observed command = %#v, producer=%q", observedNode, observedProducer)
			}
		} else if !strings.HasPrefix(stateOutput.Path, compactKbuildSideOutputStateDirectory+"/commands/") {
			t.Fatalf("observed command %d state output = %#v, want private command state", commandIndex, stateOutput)
		}
		bases := observedRecipe.ObservedOutputBases[stateBinding]
		if commandIndex == 0 {
			if len(bases) != 0 {
				t.Fatalf("first observed command bases = %q, want candidate frontier only", bases)
			}
		} else {
			if len(bases) != 1 {
				t.Fatalf("observed command %d bases = %q, want previous command state", commandIndex, bases)
			}
			inputIndex := slices.Index(observedRecipe.Inputs, bases[0])
			if inputIndex < 0 || inputIndex >= len(observedNode.Inputs) {
				t.Fatalf("observed command %d base binding %q is not an input: node=%#v recipe=%#v", commandIndex, bases[0], observedNode, observedRecipe)
			}
			base := observedNode.Inputs[inputIndex]
			if base.Role != "observed-state" || base.ProducerID != previousProducer || base.Slot != previousStateSlot {
				t.Fatalf("observed command %d base = %#v, want %s slot %d", commandIndex, base, previousProducer, previousStateSlot)
			}
		}
		previousProducer, previousStateSlot = observedNode.ID, stateSlot
	}
	if _, err := observedPlan.entries(); err != nil {
		t.Fatalf("observed multi-command filechk plan is invalid: %v", err)
	}
}

func TestGenericKbuildRecipeLowersLiteralScalarPipeline(t *testing.T) {
	payload := "#7 SMP PREEMPT_DYNAMIC Thu Aug 14 03:20:00 UTC 2026 source-selected-suffix"
	value := `utsver=$(echo '` + payload + `' | cut -b -64); echo '#define' UTS_VERSION \""${utsver}"\"`
	lines, literal, err := parseCompactKbuildLiteralFilechk(value, compactKbuildAutomaticContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !literal {
		t.Fatal("literal scalar pipeline was not recognized")
	}
	wantPayload := payload
	if len(wantPayload) > 64 {
		wantPayload = wantPayload[:64]
	}
	want := []string{`#define UTS_VERSION "` + wantPayload + `"`}
	if !slices.Equal(lines, want) {
		t.Fatalf("literal scalar pipeline lines = %q, want %q", lines, want)
	}
}

func TestGenericKbuildRecipeLowersUtsreleaseLengthGuardWithoutShell(t *testing.T) {
	makefile := strings.ReplaceAll(`
KERNELRELEASE = 6.18.39-test
uts_len := 64
define filechk_utsrelease.h
	if [ __BACKTICK__echo -n "$(KERNELRELEASE)" | wc -c __BACKTICK__ -gt $(uts_len) ]; then \
	  echo '"$(KERNELRELEASE)" exceeds $(uts_len) characters' >&2; \
	  exit 1; \
	fi; \
	echo \#define UTS_RELEASE \"$(KERNELRELEASE)\"
endef
include/generated/utsrelease.h: FORCE
	$(call filechk,utsrelease.h)
`, "__BACKTICK__", "`")
	profile := mustCompactKbuildProfileForTest(t, "prep:utsrelease", "Makefile", "", makefile, nil)
	target := "include/generated/utsrelease.h"
	metadata := &CompactMetadata{
		actionRoles: testConfiguredScopedActionRoles,
		Config:      CompactConfig{KbuildProfiles: []CompactKbuildProfile{profile}},
	}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	if err := buildCompactKbuildTargetForTest(metadata, plan, target); err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 1; got != want {
		t.Fatalf("node count=%d, want %d: %#v", got, want, plan.Nodes)
	}
	node := plan.Nodes[0]
	recipe := plan.Recipes[node.Recipe]
	if node.Tool != "actionfile" || recipe.Tool != "actionfile" || node.Outputs[0].Path != target {
		t.Fatalf("literal filechk node=%#v recipe=%#v", node, recipe)
	}
	want := "#define UTS_RELEASE \"6.18.39-test\""
	if got := slices.Index(recipe.Arguments, "-line"); got < 0 || got+1 == len(recipe.Arguments) || recipe.Arguments[got+1] != want {
		t.Fatalf("literal filechk arguments=%q, want line %q", recipe.Arguments, want)
	}
}

func BenchmarkCompoundWorkingInputPathsCumulativeFrontier(b *testing.B) {
	for _, count := range []int{128, 256, 512} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			parsed, err := parseKbuildWithOptions(strings.NewReader(""), "Makefile", KbuildOptions{
				CaptureTargetEvaluator: true,
			}, "")
			if err != nil {
				b.Fatal(err)
			}
			profile, err := NewCompactKbuildProfile("working-input-benchmark", "Makefile", "", parsed)
			if err != nil {
				b.Fatal(err)
			}
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: CompactKbuildInvocationObjectTree,
			}); err != nil {
				b.Fatal(err)
			}
			const (
				objectRoot = "__LINUX_BZL_OBJECT_TREE__/"
				source     = "drivers/performance/working-input/input.c"
				config     = "include/generated/autoconf.h"
				fixdep     = "scripts/basic/fixdep"
			)
			inputs := []compactKbuildRuleInput{
				{path: source, sourceID: "src-00000001"},
				{path: config, producer: "config-producer"},
				{path: fixdep, producer: "fixdep-producer"},
			}
			commands := make([][]compactKbuildRecipeCommand, count)
			for index := range commands {
				target := fmt.Sprintf("drivers/performance/working-input/unit-%04d.o", index)
				depfile := objectRoot + target + ".d"
				compile := KbuildActionRoleToken("target", "cc") +
					" -nostdinc -O2 -Wall -Werror -fno-common -fno-pie -fno-strict-aliasing" +
					" -D__KERNEL__ -DKBUILD_MODNAME=benchmark -DKBUILD_BASENAME=unit" +
					" -include " + objectRoot + config + " -Wp,-MMD," + depfile +
					" -c __LINUX_BZL_SOURCE_TREE__/" + source + " -o " + objectRoot + target
				template := compile + "; " + objectRoot + fixdep + " " + depfile + " " +
					objectRoot + target + " " + compactKbuildShellLiteralWord(compile)
				commands[index], err = compactKbuildCompoundProgramCommands(template)
				if err != nil {
					b.Fatal(err)
				}
				if !compactKbuildCompoundWorkingInputUsesComplete(profile, target, commands[index]) {
					b.Fatal("benchmark command does not satisfy the closed working-input grammar")
				}
				inputs = append(inputs, compactKbuildRuleInput{path: target, producer: fmt.Sprintf("producer-%04d", index)})
			}
			// Each operation is a batch of ordinary compile+fixdep actions. The
			// staged frontier retains outputs from earlier actions, but argv size
			// stays fixed. Construction, parsing, grammar checks, and warming the
			// profile's source-path cache are excluded from measurement.
			for index, command := range commands {
				used := compactKbuildCompoundWorkingInputPathsNaiveForTest(profile, command, inputs[:3+index])
				if len(used) != 3 || !used[source] || !used[config] || !used[fixdep] {
					b.Fatalf("command %d selected unexpected inputs: %#v", index, used)
				}
				indexed := compactKbuildCompoundWorkingInputPaths(profile, command, inputs[:3+index])
				if !maps.Equal(indexed, used) {
					b.Fatalf("command %d indexed inputs %#v differ from naive %#v", index, indexed, used)
				}
			}
			for _, implementation := range []struct {
				name  string
				query func(CompactKbuildProfile, []compactKbuildRecipeCommand, []compactKbuildRuleInput) map[string]bool
			}{
				{name: "naive", query: compactKbuildCompoundWorkingInputPathsNaiveForTest},
				{name: "indexed", query: compactKbuildCompoundWorkingInputPaths},
			} {
				b.Run(implementation.name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for iteration := 0; iteration < b.N; iteration++ {
						for index, command := range commands {
							used := implementation.query(profile, command, inputs[:3+index])
							if len(used) != 3 {
								b.Fatalf("command %d selected %d inputs, want 3", index, len(used))
							}
						}
					}
					b.ReportMetric(float64(count), "actions/op")
				})
			}
		})
	}
}

func TestCompoundWorkingInputPathsIndexedMatchesNaive(t *testing.T) {
	const (
		objectRoot = "__LINUX_BZL_OBJECT_TREE__/"
		sourceRoot = "__LINUX_BZL_SOURCE_TREE__/"
	)
	cc := KbuildActionRoleToken("target", "cc")
	for _, test := range []struct {
		name, command string
		inputs, want  []string
		complete      bool
	}{
		{
			name:    "raw non-component and multiple suffixes",
			command: cc + " -c " + sourceRoot + "drivers/foobar.c -o " + objectRoot + "out",
			inputs:  []string{"drivers/foobar.c", "foobar.c", "bar.c", "ar.c", "other.c"},
			want:    []string{"drivers/foobar.c", "foobar.c", "bar.c", "ar.c"}, complete: true,
		},
		{
			name:    "UTF-8 suffix byte lengths",
			command: cc + " -c " + sourceRoot + "drivers/模块.c -o " + objectRoot + "out",
			inputs:  []string{"drivers/模块.c", "模块.c", "块.c", ".c", "é.c"},
			want:    []string{"drivers/模块.c", "模块.c", "块.c", ".c"}, complete: true,
		},
		{
			name:    "canonicalized duplicate and empty input paths",
			command: cc + " -include " + sourceRoot + "include/forced.h -c " + sourceRoot + "drivers/input.c -o " + objectRoot + "out",
			inputs:  []string{"./drivers/./input.c", " drivers/cache/../input.c ", "include/generated/../forced.h", "", "."},
			want:    []string{"drivers/input.c", "include/forced.h"}, complete: true,
		},
		{
			name:    "response operand remains conservative",
			command: cc + " @" + objectRoot + "options.rsp -c " + sourceRoot + "source.c -o " + objectRoot + "out",
			inputs:  []string{"options.rsp", "ions.rsp", "source.c"},
			want:    []string{"options.rsp", "ions.rsp", "source.c"},
		},
		{
			name:    "comma and equal operand splits",
			command: cc + " -Wp,-include," + sourceRoot + "forced.h -DHEADER=" + sourceRoot + "config.h -c " + sourceRoot + "source.c -o " + objectRoot + "out",
			inputs:  []string{"forced.h", "config.h", "source.c", "unused.h"},
			want:    []string{"forced.h", "config.h", "source.c"}, complete: true,
		},
		{
			name:    "script and forced-header prefixes",
			command: "echo -T" + sourceRoot + "first.lds --script=" + sourceRoot + "second.lds -include" + sourceRoot + "forced.h -imacros" + sourceRoot + "macros.h",
			inputs:  []string{"first.lds", "second.lds", "forced.h", "macros.h"},
			want:    []string{"first.lds", "second.lds", "forced.h", "macros.h"}, complete: true,
		},
		{
			name:    "environment stdin and exact program binding",
			command: "DATA=" + sourceRoot + "env.h " + objectRoot + "scripts/basic/fixdep " + objectRoot + "file.d " + objectRoot + "out saved < " + sourceRoot + "stdin.h",
			inputs:  []string{"env.h", "stdin.h", "file.d", "scripts/basic/fixdep", "fixdep"},
			want:    []string{"env.h", "stdin.h", "file.d", "scripts/basic/fixdep"},
		},
		{
			name:    "static compiler stdin",
			command: cc + " -c " + sourceRoot + "source.c -o " + objectRoot + "out < " + sourceRoot + "stdin.h",
			inputs:  []string{"source.c", "stdin.h"}, want: []string{"source.c", "stdin.h"}, complete: true,
		},
		{
			name:    "private source and object tree markers",
			command: cc + " -include " + compactKbuildActionObjectTreeMarker + "/forced.h -c " + compactKbuildActionSourceTreeMarker + "/source.c -o " + compactKbuildActionObjectTreeMarker + "/out",
			inputs:  []string{"source.c", "forced.h"}, want: []string{"source.c", "forced.h"}, complete: true,
		},
		{
			name:    "trailing slash suffixes",
			command: "echo /prefix/directory/",
			inputs:  []string{"directory/", "tory/", "ory/", "directory"},
			want:    []string{"directory/", "tory/", "ory/"}, complete: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := CompactKbuildProfile{Name: "indexed-working-input-equivalence"}
			if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
				Tree: CompactKbuildInvocationObjectTree,
			}); err != nil {
				t.Fatal(err)
			}
			commands, err := compactKbuildCompoundProgramCommands(test.command)
			if err != nil {
				t.Fatal(err)
			}
			if complete := compactKbuildCompoundWorkingInputUsesComplete(profile, "out", commands); complete != test.complete {
				t.Fatalf("closed working-input grammar = %v, want %v", complete, test.complete)
			}
			inputs := make([]compactKbuildRuleInput, len(test.inputs))
			for index, pathname := range test.inputs {
				inputs[index].path = pathname
			}
			want := make(map[string]bool, len(test.want))
			for _, pathname := range test.want {
				want[pathname] = true
			}
			naive := compactKbuildCompoundWorkingInputPathsNaiveForTest(profile, commands, inputs)
			if !maps.Equal(naive, want) {
				t.Fatalf("naive matcher = %#v, want %#v", naive, want)
			}
			indexed := compactKbuildCompoundWorkingInputPaths(profile, commands, inputs)
			if !maps.Equal(indexed, naive) {
				t.Fatalf("indexed matcher = %#v, naive = %#v", indexed, naive)
			}
		})
	}
}

// Retain the original full-frontier scan as an independent equivalence and
// benchmark oracle. In particular, its raw suffix match is not restricted to
// directory-component boundaries and may select several overlapping paths.
func compactKbuildCompoundWorkingInputPathsNaiveForTest(
	profile CompactKbuildProfile,
	commands []compactKbuildRecipeCommand,
	inputs []compactKbuildRuleInput,
) map[string]bool {
	inputPaths := make(map[string]bool, len(inputs))
	for _, input := range inputs {
		if pathname := canonicalKbuildRulePath(input.path); pathname != "" {
			inputPaths[pathname] = true
		}
	}
	used := map[string]bool{}
	add := func(pathname string) {
		pathname = canonicalKbuildRulePath(pathname)
		if inputPaths[pathname] {
			used[pathname] = true
		}
	}
	resolveOperand := func(value string) {
		values := []string{value}
		values = append(values, strings.FieldsFunc(value, func(character rune) bool {
			return character == ',' || character == '='
		})...)
		baseValues := slices.Clone(values)
		for _, candidate := range baseValues {
			candidate = strings.TrimPrefix(candidate, "@")
			for _, prefix := range []string{"-T", "--script=", "-include", "-imacros"} {
				if strings.HasPrefix(candidate, prefix) && len(candidate) > len(prefix) {
					values = append(values, candidate[len(prefix):])
				}
			}
		}
		for _, candidate := range values {
			candidate = strings.TrimPrefix(candidate, "@")
			if pathname, ok := compactKbuildProfileCommandOperandPath(profile, candidate); ok {
				add(pathname)
			}
			if pathname, _, ok := compactKbuildProfileCommandPath(profile, candidate); ok {
				add(pathname)
			}
			add(compactKbuildCompilerSourceOperandGraphPath(candidate))
			materialized := compactKbuildMaterializeActionTreeMarkers(candidate)
			for pathname := range inputPaths {
				if materialized == pathname || strings.HasSuffix(materialized, "/"+pathname) || strings.HasSuffix(materialized, pathname) {
					used[pathname] = true
				}
			}
		}
	}
	for _, command := range commands {
		if pathname, _, ok := compactKbuildProfileCommandPath(profile, command.program); ok {
			add(pathname)
		}
		for _, argument := range command.arguments {
			resolveOperand(argument)
		}
		if command.stdin != "" {
			resolveOperand(command.stdin)
		}
		for _, name := range sortedStringMapKeys(command.environment) {
			resolveOperand(command.environment[name])
		}
	}
	return used
}
