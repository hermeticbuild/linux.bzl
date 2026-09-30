package kconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func compactKbuildObjectTreeScriptProfileForTest(
	t *testing.T,
	scripts map[string]string,
) CompactKbuildProfile {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"Makefile": `
CONFIG_SHELL = sh
export OBSERVED_ROOT = $(objtree)
export UNUSED_ROOT = $(objtree)
export DYNAMIC_NAME = generated
all:
	@true
`,
	}
	for name, content := range scripts {
		files[name] = content
	}
	for name, content := range files {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	kb, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), KbuildOptions{
		RootDir: root,
		Variables: map[string]string{
			"objtree": "__LINUX_BZL_OBJECT_TREE__",
			"srctree": "__LINUX_BZL_SOURCE_TREE__",
		},
		SourceRoots: map[string]string{
			"__LINUX_BZL_OBJECT_TREE__": root,
			"__LINUX_BZL_SOURCE_TREE__": root,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root", filepath.Join(root, "Makefile"), root, kb)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func compactKbuildObjectTreeObservationForTest(
	t *testing.T,
	profile CompactKbuildProfile,
	command string,
) CompactKbuildObjectTreeObservation {
	t.Helper()
	observation, err := EvaluateCompactKbuildSourceScriptObjectTreeObservationSymbolic(
		profile,
		"generated/result.h",
		"",
		nil,
		nil,
		map[string]string{
			"objtree": "__LINUX_BZL_OBJECT_TREE__",
			"srctree": "__LINUX_BZL_SOURCE_TREE__",
		},
		command,
	)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func TestPreconfiguredObjectTreeProvidesResolvedConfigScriptBaseline(t *testing.T) {
	objectRoot := t.TempDir()
	exact := map[string]string{}
	for _, pathname := range recognizedConfigDocuments() {
		mustWriteSource(t, objectRoot, pathname, "preconfigured\n")
		exact[pathname] = "prep"
	}
	profile := compactKbuildObjectTreeScriptProfileForTest(t, nil)
	profile.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_OBJECT_TREE__": objectRoot}
	metadata := &CompactMetadata{configProjectionPaths: recognizedConfigDocuments(),
		preconfiguredObjectTree: true,
		exactSourceNamespaces:   exact,
	}
	plan := &ActionPlan{}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).forProfile(profile)
	baseline, err := builder.compactKbuildWorkingTreeClosureInputs("generated/result.h", profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(baseline), len(exact); got != want {
		t.Fatalf("resolved-config baseline has %d inputs, want %d: %#v", got, want, baseline)
	}
	seen := map[string]bool{}
	for _, input := range baseline {
		if !input.workingOnly || !input.objectTree || input.sourceID == "" || input.producer != "" {
			t.Fatalf("resolved-config baseline input is not an immutable working object-tree leaf: %#v", input)
		}
		seen[input.path] = true
	}
	for pathname := range exact {
		if !seen[pathname] {
			t.Errorf("resolved-config baseline omitted %q", pathname)
		}
	}
	for _, source := range plan.Sources {
		_, exists := exact[source.Path]
		if source.Namespace != "prep" || !exists {
			t.Errorf("resolved-config source routed incorrectly: %#v", source)
		}
	}
}

func TestSourceScriptObjectTreeObservationIgnoresUnrelatedScript(t *testing.T) {
	profile := compactKbuildObjectTreeScriptProfileForTest(t, map[string]string{
		"scripts/unrelated.sh": "#!/bin/sh\nprintf '%s\\n' unrelated\n",
	})
	got := compactKbuildObjectTreeObservationForTest(
		t, profile, `sh ${tree:kernel}/scripts/unrelated.sh`,
	)
	if got.ObservesObjectTree || got.ObservesAll || len(got.References) != 0 {
		t.Fatalf("unrelated script observation = %#v, want none", got)
	}
}

func TestSourceScriptObjectTreeObservationReadsSelectedMkcompileVersion(t *testing.T) {
	const mkcompile = `#!/bin/sh
TARGET=$1
if [ -z "$KBUILD_BUILD_VERSION" ]; then
	VERSION=$(cat .version 2>/dev/null || echo 1)
else
	VERSION=$KBUILD_BUILD_VERSION
fi
printf '%s\\n' "$VERSION" > "$TARGET"
`
	profile := compactKbuildObjectTreeScriptProfileForTest(t, map[string]string{
		"scripts/mkcompile_h": mkcompile,
	})
	command := `sh ${tree:kernel}/scripts/mkcompile_h include/generated/compile.h`
	want := []string{".version"}
	if got := compactKbuildObjectTreeObservationForTest(t, profile, command); !got.ObservesObjectTree ||
		got.ObservesAll || !reflect.DeepEqual(got.References, want) {
		t.Fatalf("selected mkcompile_h observation = %#v, want %q", got, want)
	}
	// The lexical obj= directory is not the child Make process's cwd. A
	// recursive Makefile.build obj=init still reads the root .version.
	profile.Directory = "init"
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	if got := compactKbuildObjectTreeObservationForTest(t, profile, command); !reflect.DeepEqual(got.References, want) {
		t.Fatalf("recursive init invocation observed %#v, want root .version", got)
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: "other",
	}); err != nil {
		t.Fatal(err)
	}
	if got := compactKbuildObjectTreeObservationForTest(t, profile, command); !reflect.DeepEqual(got.References, []string{"other/.version"}) {
		t.Fatalf("nested invocation observed %#v, want other/.version", got)
	}
	for _, version := range []string{"1", "custom-build"} {
		got := compactKbuildObjectTreeObservationForTest(
			t, profile, `KBUILD_BUILD_VERSION=`+version+` `+command,
		)
		if got.ObservesObjectTree || got.ObservesAll || len(got.References) != 0 {
			t.Errorf("version %q observation = %#v, want no .version read", version, got)
		}
	}
	if got := compactKbuildObjectTreeObservationForTest(t, profile,
		`KBUILD_BUILD_VERSION= `+command); !reflect.DeepEqual(got.References, []string{"other/.version"}) {
		t.Fatalf("empty version observation = %#v, want relative object read", got)
	}
}

func TestSourceScriptObjectTreeObservationRejectsChangedMkcompileVersion(t *testing.T) {
	const block = `#!/bin/sh
if [ -z "$KBUILD_BUILD_VERSION" ]; then
	VERSION=$(cat .version 2>/dev/null || echo 1)
else
	VERSION=$KBUILD_BUILD_VERSION
fi
`
	for _, tc := range []struct {
		name, content string
		wantRead      bool
		wantError     bool
	}{
		{name: "canonical", content: block, wantRead: true},
		{name: "alternate source read", content: strings.Replace(block, "cat .version", "cat .version.new", 1), wantError: true},
		{name: "changed conditional", content: strings.Replace(block, `-z "$KBUILD_BUILD_VERSION"`, `-n "$KBUILD_BUILD_VERSION"`, 1), wantError: true},
		{name: "additional read", content: block + "cat .version\n", wantError: true},
		{name: "comment only", content: "#!/bin/sh\n# cat .version\n", wantRead: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := compactKbuildObjectTreeScriptProfileForTest(t, map[string]string{
				"scripts/mkcompile_h": tc.content,
			})
			observed, err := EvaluateCompactKbuildSourceScriptObjectTreeObservationSymbolic(
				profile, "include/generated/compile.h", "", nil, nil,
				map[string]string{"objtree": "__LINUX_BZL_OBJECT_TREE__", "srctree": "__LINUX_BZL_SOURCE_TREE__"},
				`sh ${tree:kernel}/scripts/mkcompile_h include/generated/compile.h`,
			)
			if (err != nil) != tc.wantError {
				t.Fatalf("observation error = %v, want error %t (result %#v)", err, tc.wantError, observed)
			}
			if err == nil && observed.ObservesObjectTree != tc.wantRead {
				t.Fatalf("observation = %#v, want read %t", observed, tc.wantRead)
			}
		})
	}
}

func TestSourceScriptObjectTreeObservationIgnoresNonScriptRecipeSyntax(t *testing.T) {
	profile := compactKbuildObjectTreeScriptProfileForTest(t, nil)
	for _, command := range []string{
		`printf '%s\n' "$$value"`,
		KbuildActionRoleToken("host", "cc") + " " + KbuildActionRoleToken("target", "cc"),
		`echo setup; ;`,
	} {
		got := compactKbuildObjectTreeObservationForTest(t, profile, command)
		if got.ObservesObjectTree || got.ObservesAll || len(got.References) != 0 {
			t.Errorf("non-script command %q observation = %#v, want none", command, got)
		}
	}
}

func TestSourceScriptObjectTreeObservationScopesEnvironmentAndArguments(t *testing.T) {
	profile := compactKbuildObjectTreeScriptProfileForTest(t, map[string]string{
		"scripts/scoped.sh":   "#!/bin/sh\nprintf '%s\\n' \"${OBSERVED_ROOT}/include/generated/asm/types.h\"\n",
		"scripts/argument.sh": "#!/bin/sh\nprintf '%s\\n' argument\n",
		"scripts/inline.sh":   "#!/bin/sh\nprintf '%s\\n' \"${INLINE_ROOT}/autoconf.h\"\n",
	})
	for _, test := range []struct {
		name       string
		command    string
		references []string
	}{
		{
			name:       "exported environment path",
			command:    `sh ${tree:kernel}/scripts/scoped.sh`,
			references: []string{"include/generated/asm/types.h"},
		},
		{
			name:       "explicit argument",
			command:    `sh ${tree:kernel}/scripts/argument.sh ${tree:prep}/tools/objtool/objtool`,
			references: []string{"tools/objtool/objtool"},
		},
		{
			name:       "valued interpreter options",
			command:    `sh -eo pipefail +O extglob ${tree:kernel}/scripts/argument.sh ${tree:prep}/tools/objtool/objtool -c input.c`,
			references: []string{"tools/objtool/objtool"},
		},
		{
			name:       "inline environment path",
			command:    `INLINE_ROOT=${tree:prep}/include/generated sh ${tree:kernel}/scripts/inline.sh`,
			references: []string{"include/generated/autoconf.h"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := compactKbuildObjectTreeObservationForTest(t, profile, test.command)
			if !got.ObservesObjectTree || got.ObservesAll || !reflect.DeepEqual(got.References, test.references) {
				t.Fatalf("observation = %#v, want scoped references %q", got, test.references)
			}
		})
	}
}

func TestSourceScriptObjectTreeObservationKeepsScriptInsideControlFlow(t *testing.T) {
	profile := compactKbuildObjectTreeScriptProfileForTest(t, map[string]string{
		"scripts/inline.sh": "#!/bin/sh\nprintf '%s\\n' \"${INLINE_ROOT}/vdso.so\"\n",
	})
	got := compactKbuildObjectTreeObservationForTest(
		t,
		profile,
		`INLINE_ROOT=${tree:prep}/arch/x86 sh ${tree:kernel}/scripts/inline.sh; if readelf -rW output | grep -q relocation; then rm -f output; fi`,
	)
	want := []string{"arch/x86/vdso.so"}
	if !got.ObservesObjectTree || got.ObservesAll || !reflect.DeepEqual(got.References, want) {
		t.Fatalf("control-flow observation = %#v, want %q", got, want)
	}
}

func TestSourceScriptObjectTreeObservationConsumesEvaluatedAutomaticInsideControlFlow(t *testing.T) {
	profile := compactKbuildObjectTreeScriptProfileForTest(t, map[string]string{
		"scripts/argument.sh": "#!/bin/sh\nprintf '%s\\n' \"$1\" \"$2\"\n",
	})
	const target = "generated/result.h"
	injected := map[string]string{
		"objtree": "__LINUX_BZL_OBJECT_TREE__",
		"srctree": "__LINUX_BZL_SOURCE_TREE__",
	}
	evaluated, _, err := EvaluateCompactKbuildTextActionRolesForMakeTarget(
		profile,
		target,
		target,
		"__LINUX_BZL_OBJECT_TREE__/"+target,
		"",
		[]string{"__LINUX_BZL_OBJECT_TREE__/generated/input.h"},
		nil,
		injected,
		`sh $(srctree)/scripts/argument.sh '$<' "$$@"; if readelf -rW output | grep -q relocation; then rm -f output; fi`,
	)
	if err != nil {
		t.Fatal(err)
	}
	evaluated, err = ResolveCompactKbuildTargetSymbolicText(profile, target, evaluated)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(evaluated, "$<") ||
		!strings.Contains(evaluated, "${tree:prep}/generated/input.h") ||
		!strings.Contains(evaluated, `"$@"`) {
		t.Fatalf("target-evaluated command = %q, want expanded Make input and retained shell $@", evaluated)
	}
	observation, err := EvaluateCompactKbuildSourceScriptObjectTreeObservationSymbolicForMakeTarget(
		profile,
		target,
		target,
		"__LINUX_BZL_OBJECT_TREE__/"+target,
		"",
		[]string{"__LINUX_BZL_OBJECT_TREE__/generated/input.h"},
		nil,
		injected,
		evaluated,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"generated/input.h"}
	if !observation.ObservesObjectTree || observation.ObservesAll || !reflect.DeepEqual(observation.References, want) {
		t.Fatalf("automatic control-flow observation = %#v, want %q", observation, want)
	}
}

func TestSourceScriptObjectTreeObservationResolvesExactReplayEnvironment(t *testing.T) {
	const token = "LINUX_BZL_PROBE_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(`
CONFIG_SHELL = sh
export OBSERVED_ROOT = `+token+`
all:
	@true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "scripts", "observe.sh"),
		[]byte("#!/bin/sh\nprintf '%s\\n' \"${OBSERVED_ROOT}/include/generated/asm/types.h\"\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKbuildFileTree(filepath.Join(root, "Makefile"), KbuildOptions{
		RootDir: root,
		Variables: map[string]string{
			"objtree": "__LINUX_BZL_OBJECT_TREE__",
			"srctree": "__LINUX_BZL_SOURCE_TREE__",
		},
		SourceRoots: map[string]string{
			"__LINUX_BZL_OBJECT_TREE__": root,
			"__LINUX_BZL_SOURCE_TREE__": root,
		},
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
		ResolveSymbolic: func(value string) (string, error) {
			return strings.ReplaceAll(value, token, "__LINUX_BZL_OBJECT_TREE__"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("exact-script-environment", filepath.Join(root, "Makefile"), root, parsed)
	if err != nil {
		t.Fatal(err)
	}
	injected := map[string]string{
		"objtree": "__LINUX_BZL_OBJECT_TREE__",
		"srctree": "__LINUX_BZL_SOURCE_TREE__",
	}
	command := `sh ${tree:kernel}/scripts/observe.sh`
	symbolic, err := EvaluateCompactKbuildSourceScriptObjectTreeObservationSymbolic(
		profile, "all", "", nil, nil, injected, command,
	)
	if err != nil {
		t.Fatal(err)
	}
	if symbolic.ObservesObjectTree || symbolic.ObservesAll || len(symbolic.References) != 0 {
		t.Fatalf("symbolic source-script environment observation = %#v, want hidden marker", symbolic)
	}
	exact, err := EvaluateCompactKbuildSourceScriptObjectTreeObservation(
		profile, "all", "", nil, nil, injected, command,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"include/generated/asm/types.h"}
	if !exact.ObservesObjectTree || exact.ObservesAll || !reflect.DeepEqual(exact.References, want) {
		t.Fatalf("exact source-script environment observation = %#v, want %q", exact, want)
	}
}

func TestSourceScriptObjectTreeObservationBareAndDynamicRootsObserveAll(t *testing.T) {
	profile := compactKbuildObjectTreeScriptProfileForTest(t, map[string]string{
		"scripts/bare.sh":    "#!/bin/sh\nprintf '%s\\n' \"$OBSERVED_ROOT\"\n",
		"scripts/dynamic.sh": "#!/bin/sh\nprintf '%s\\n' \"${OBSERVED_ROOT}/${DYNAMIC_NAME}\"\n",
	})
	for _, script := range []string{"bare", "dynamic"} {
		t.Run(script, func(t *testing.T) {
			got := compactKbuildObjectTreeObservationForTest(
				t, profile, `sh ${tree:kernel}/scripts/`+script+`.sh`,
			)
			if !got.ObservesObjectTree || !got.ObservesAll || len(got.References) != 0 {
				t.Fatalf("observation = %#v, want all object-tree paths", got)
			}
		})
	}
}

func TestObjectTreeObservationIgnoresPassivePrefixMapsButKeepsReadOperands(t *testing.T) {
	for _, option := range []string{
		"-fmacro-prefix-map",
		"-ffile-prefix-map",
		"-fdebug-prefix-map",
		"--debug-prefix-map",
	} {
		t.Run(option, func(t *testing.T) {
			observation := ObserveCompactKbuildObjectTree(
				KbuildActionRoleToken("target", "cc") +
					" " + option + "=${tree:prep}/=. -I ${tree:prep} -include ${tree:prep}/include/generated/autoconf.h",
			)
			want := []string{"include/generated/autoconf.h"}
			if !observation.ObservesObjectTree || observation.ObservesAll || !reflect.DeepEqual(observation.References, want) {
				t.Fatalf("prefix-map plus read observation = %#v, want exact references %q", observation, want)
			}
		})
	}
	nonCompiler := ObserveCompactKbuildObjectTree("opaque-tool -I ${tree:prep}")
	if !nonCompiler.ObservesObjectTree || !nonCompiler.ObservesAll {
		t.Fatalf("opaque -I root observation = %#v, want conservative all-visible", nonCompiler)
	}
}

func TestObjectTreeObservationDistinguishesCompilerOutputFromLaterRead(t *testing.T) {
	const output = "tools/objtool/fixdep"
	const input = "tools/objtool/fixdep-in.o"
	compiler := KbuildActionRoleToken("host", "cc")
	for _, tc := range []struct {
		name, flags, suffix string
		want                []string
	}{
		{name: "separated output", flags: "-o ${tree:prep}/" + output, want: []string{input}},
		{name: "joined output", flags: "-o${tree:prep}/" + output, want: []string{input}},
		{name: "later command reads output", flags: "-o ${tree:prep}/" + output, suffix: "; cat ${tree:prep}/" + output, want: []string{output, input}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation := ObserveCompactKbuildObjectTree(
				compiler + " -r " + tc.flags + " ${tree:prep}/" + input + tc.suffix,
			)
			if !observation.ObservesObjectTree || observation.ObservesAll || !reflect.DeepEqual(observation.References, tc.want) {
				t.Fatalf("compiler output/read observation = %#v, want exact reads %q", observation, tc.want)
			}
		})
	}
	opaque := ObserveCompactKbuildObjectTree("opaque-tool -o ${tree:prep} input.o")
	if !opaque.ObservesObjectTree || !opaque.ObservesAll {
		t.Fatalf("unconfigured tool output observation = %#v, want conservative all-visible", opaque)
	}
}

func TestObjectTreeObservationKeepsHostLinkerInputsWithoutReadingItsOutput(t *testing.T) {
	const output = "tools/objtool/fixdep-in.o"
	const input = "tools/objtool/fixdep.o"
	linker := KbuildActionRoleToken("host", "ld")
	for _, tc := range []struct {
		name, suffix string
		want         []string
	}{
		{name: "source quiet link and selected host linker", want: []string{input}},
		{name: "later source command reads linker output", suffix: "; cat ${tree:prep}/" + output, want: []string{output, input}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := "echo '  LINK     '${tree:prep}/" + output + "; " + linker +
				" -r -o ${tree:prep}/" + output + " ${tree:prep}/" + input + tc.suffix
			observation := ObserveCompactKbuildObjectTree(command)
			if !observation.ObservesObjectTree || observation.ObservesAll || !reflect.DeepEqual(observation.References, tc.want) {
				t.Fatalf("source host-link output/read observation = %#v, want exact inputs %q", observation, tc.want)
			}
		})
	}
	for _, argument := range []string{linker, KbuildActionRoleToken("host", "cc")} {
		foreign := ObserveCompactKbuildObjectTree("selected-source-script -r -o ${tree:prep}/" + output + " " + argument)
		if !foreign.ObservesObjectTree || foreign.ObservesAll || !reflect.DeepEqual(foreign.References, []string{output}) {
			t.Fatalf("role token %q supplied as an argument to unconfigured tool = %#v, want exact destination read", argument, foreign)
		}
	}
}

func TestSelectedPlainLinkerOutputIsNotAnObjectTreeRead(t *testing.T) {
	const output = "tools/objtool/fixdep-in.o"
	const input = "tools/objtool/fixdep.o"
	profile := mustCompactKbuildProfileForTest(t, "build:fixdep", "tools/build/Makefile.build", "", `
$(OUTPUT)fixdep-in.o: $(OUTPUT)fixdep.o FORCE
	$(call if_changed,host_ld_multi)
`, nil)
	profile.evaluator.template.actionRoles = []KbuildActionRoleRef{
		{Scope: "host", Role: "ld"}, {Scope: "target", Role: "ld"},
	}
	unconfigured := mustCompactKbuildProfileForTest(t, "build:unconfigured", "tools/build/Makefile.build", "", `
$(OUTPUT)fixdep-in.o: $(OUTPUT)fixdep.o FORCE
	$(call if_changed,host_ld_multi)
`, nil)
	targetOnly := mustCompactKbuildProfileForTest(t, "build:target-only", "tools/build/Makefile.build", "", `
$(OUTPUT)fixdep-in.o: $(OUTPUT)fixdep.o FORCE
	$(call if_changed,host_ld_multi)
`, nil)
	targetOnly.evaluator.template.actionRoles = []KbuildActionRoleRef{{Scope: "target", Role: "ld"}}
	link := "ld -r -o __LINUX_BZL_OBJECT_TREE__/" + output + " __LINUX_BZL_OBJECT_TREE__/" + input
	prepLink := "ld -r -o ${tree:prep}/" + output + " ${tree:prep}/" + input
	for _, tc := range []struct {
		name       string
		command    string
		configured bool
		targetOnly bool
		want       []string
	}{
		{name: "selected literal host linker", command: link, configured: true, want: []string{input}},
		{name: "source if_changed wrapper", command: "@set -e; echo '  HOSTLD   __LINUX_BZL_OBJECT_TREE__/" + output + "'; " +
			link + "; printf '%s\\n' 'cmd_" + output + " := " + link + "' > __LINUX_BZL_OBJECT_TREE__/tools/objtool/.fixdep-in.o.cmd", configured: true, want: []string{input}},
		{name: "selected prep-root linker wrapper", command: "@set -e; " + prepLink +
			"; printf '%s\\n' 'cmd_${tree:prep}/" + output + " := " + prepLink +
			"' > ${tree:prep}/tools/objtool/.fixdep-in.o.cmd", configured: true, want: []string{input}},
		{name: "unconfigured literal program", command: link, want: []string{output, input}},
		{name: "linker configured in only one scope", command: link, targetOnly: true, want: []string{output, input}},
		{name: "unknown output flag program", command: "unknown -r -o __LINUX_BZL_OBJECT_TREE__/" + output + " __LINUX_BZL_OBJECT_TREE__/" + input, configured: true, want: []string{output, input}},
		{name: "later source read", command: link + "; cat __LINUX_BZL_OBJECT_TREE__/" + output, configured: true, want: []string{output, input}},
		{name: "source PATH assignment", command: "PATH=/source/bin " + link, configured: true, want: []string{output, input}},
		{name: "source PATH removal", command: "unset PATH; " + link, configured: true, want: []string{output, input}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := profile
			if tc.targetOnly {
				selected = targetOnly
			} else if !tc.configured {
				selected = unconfigured
			}
			observation := ObserveCompactKbuildSelectedRecipeObjectTree(selected, output, tc.command, tc.command)
			if observation.ObservesAll || !slices.Equal(observation.References, tc.want) {
				t.Fatalf("source-selected %q object-tree observation = %#v, want exact reads %q", tc.command, observation, tc.want)
			}
		})
	}
}

func TestObjectTreeObservationKeepsCompilerDependencyMetadataOutOfReadFrontier(t *testing.T) {
	const output = "tools/objtool/fixdep.o"
	const header = "include/generated/autoconf.h"
	compiler := KbuildActionRoleToken("host", "cc")
	command := compiler + " -Wp,-MD,${tree:prep}/tools/objtool/.fixdep.o.d" +
		" -Wp,-MT,${tree:prep}/" + output +
		" -include ${tree:prep}/" + header +
		" -c -o ${tree:prep}/" + output + " fixdep.c"
	observation := ObserveCompactKbuildObjectTree(command)
	if !observation.ObservesObjectTree || observation.ObservesAll || !reflect.DeepEqual(observation.References, []string{header}) {
		t.Fatalf("compiler dependency metadata observation = %#v, want only forced header %q", observation, header)
	}
	for _, targetFlag := range []string{"-MT", "-MQ"} {
		separated := ObserveCompactKbuildObjectTree(
			compiler + " " + targetFlag + " ${tree:prep}/" + output +
				" -include ${tree:prep}/" + header + " -o ${tree:prep}/" + output,
		)
		if !separated.ObservesObjectTree || separated.ObservesAll || !reflect.DeepEqual(separated.References, []string{header}) {
			t.Fatalf("split %s dependency target observation = %#v, want only forced header %q", targetFlag, separated, header)
		}
	}
	unconfigured := ObserveCompactKbuildObjectTree("unknown-tool -Wp,-MT,${tree:prep}/" + output)
	if !unconfigured.ObservesObjectTree || unconfigured.ObservesAll || !reflect.DeepEqual(unconfigured.References, []string{output}) {
		t.Fatalf("unconfigured dependency option = %#v, want conservative filename observation", unconfigured)
	}
}

func TestObjectTreeObservationDoesNotExecutePrintedCompilerCommand(t *testing.T) {
	const output = "tools/objtool/fixdep.o"
	const header = "include/generated/autoconf.h"
	compiler := KbuildActionRoleToken("host", "cc")
	metadata := "${tree:prep}/tools/objtool/.fixdep.o.cmd"
	command := "printf '%s\\n' 'cmd_${tree:prep}/" + output + " := " + compiler +
		" -include ${tree:prep}/" + header + " -o ${tree:prep}/" + output + "' >> " + metadata
	observation := ObserveCompactKbuildObjectTree(command)
	if !observation.ObservesObjectTree || observation.ObservesAll || !reflect.DeepEqual(observation.References, []string{"tools/objtool/.fixdep.o.cmd"}) {
		t.Fatalf("printed compiler command observation = %#v, want only prior appended metadata", observation)
	}
	active := ObserveCompactKbuildObjectTree("selected-source-tool '" + compiler +
		" -include ${tree:prep}/" + header + " -o ${tree:prep}/" + output + "'")
	if !active.ObservesObjectTree || active.ObservesAll && len(active.References) == 0 ||
		!active.ObservesAll && !reflect.DeepEqual(active.References, []string{header}) {
		t.Fatalf("serialized compiler passed to source tool observation = %#v, want authenticated forced header", active)
	}
}

func TestObjectTreeObservationRetainsPrintedPathsSentToArchivePipeline(t *testing.T) {
	const input = "generated/input.o"
	standalone := ObserveCompactKbuildObjectTree("printf '%s ' ${tree:prep}/" + input)
	if standalone.ObservesObjectTree || standalone.ObservesAll {
		t.Fatalf("standalone printf observation = %#v, want no file read", standalone)
	}
	pipeline := ObserveCompactKbuildObjectTree(
		"printf '%s ' ${tree:prep}/" + input +
			" | xargs ar cDPrST ${tree:prep}/generated/archive.a",
	)
	if !pipeline.ObservesObjectTree || !slices.Contains(pipeline.References, input) {
		t.Fatalf("archive pipeline observation = %#v, want printed source input %q retained", pipeline, input)
	}
}

func TestObjectTreeObservationSeparatesDisplayAndRedirectFromReads(t *testing.T) {
	const output = "tools/objtool/fixdep"
	rooted := "${tree:prep}/" + output
	for _, tc := range []struct {
		name, command string
		want          []string
		passive       bool
	}{
		{name: "echo operand", command: "echo " + rooted, passive: true},
		{name: "printf operand", command: "printf '%s\\n' " + rooted, passive: true},
		{name: "single quoted glob printed literally", command: "echo '" + rooted + "*'", passive: true},
		{name: "escaped glob printed literally", command: "echo " + rooted + "\\*", passive: true},
		{name: "source quoted concatenation", command: "echo '  LINK     '" + rooted, passive: true},
		{name: "stdout redirection", command: "echo data > " + rooted, passive: true},
		{name: "stdin redirection", command: "cat < " + rooted, want: []string{output}},
		{name: "subsequent cat", command: "echo " + rooted + "; cat " + rooted, want: []string{output}},
		{name: "active shell substitution", command: "echo \"$(cat " + rooted + ")\"", want: []string{output}},
		{name: "active glob star", command: "echo " + rooted + "*"},
		{name: "active glob question", command: "echo " + rooted + "?"},
		{name: "active glob class", command: "echo " + rooted + "[io]"},
		{name: "configured source tool named echo", command: "__LINUX_BZL_SOURCE_TREE__/tools/echo " + rooted, want: []string{output}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation := ObserveCompactKbuildObjectTree(tc.command)
			if tc.passive {
				if observation.ObservesObjectTree || observation.ObservesAll || len(observation.References) != 0 {
					t.Fatalf("passive display/write observation = %#v, want no object-tree read", observation)
				}
				return
			}
			if !observation.ObservesObjectTree ||
				(strings.HasPrefix(tc.name, "active glob") && !observation.ObservesAll) ||
				(!observation.ObservesAll && !reflect.DeepEqual(observation.References, tc.want)) {
				t.Fatalf("active or source-owned command observation = %#v, want read %q or all-visible", observation, tc.want)
			}
		})
	}
}

func TestObjectTreeObservationSanitizesSerializedCompilerCommands(t *testing.T) {
	compiler := KbuildActionRoleToken("host", "cc")
	value := compiler +
		" -I ${tree:prep}/scripts/mod -o scripts/mod/symsearch.o scripts/mod/symsearch.c; " +
		"${tree:prep}/scripts/basic/fixdep scripts/mod/.symsearch.o.d scripts/mod/symsearch.o '" +
		compiler +
		" -I ${tree:prep} -include ${tree:prep}/include/generated/autoconf.h " +
		"${tree:prep}/scripts/mod/generated-input.h -o scripts/mod/symsearch.o scripts/mod/symsearch.c' " +
		"> scripts/mod/.symsearch.o.cmd; rm -f scripts/mod/.symsearch.o.d"

	observation := ObserveCompactKbuildObjectTree(value)
	want := []string{
		"include/generated/autoconf.h",
		"scripts/basic/fixdep",
		"scripts/mod/generated-input.h",
	}
	if !observation.ObservesObjectTree || observation.ObservesAll || !reflect.DeepEqual(observation.References, want) {
		t.Fatalf("serialized compiler observation = %#v, want exact references %q", observation, want)
	}
}

func TestObjectTreeObservationKeepsOpaqueSerializedRootConservative(t *testing.T) {
	observation := ObserveCompactKbuildObjectTree(
		`opaque-tool -I ${tree:prep}`,
	)
	if !observation.ObservesObjectTree || !observation.ObservesAll || len(observation.References) != 0 {
		t.Fatalf("active opaque tool observation = %#v, want all object-tree paths", observation)
	}
}

func TestSourceScriptObjectTreeObservationPreservesForwardedCompilerCommand(t *testing.T) {
	profile := compactKbuildObjectTreeScriptProfileForTest(t, map[string]string{
		"scripts/compiler-wrapper.sh": "#!/bin/sh\nsyscall_list() { grep \"$1\"; }\ndirname \"$0\" >/dev/null\n$* -Wno-error -E -x c - >/dev/null\n",
	})
	command := "sh ${tree:kernel}/scripts/compiler-wrapper.sh " +
		KbuildActionRoleToken("target", "cc") +
		" -I ${tree:prep} -include ${tree:prep}/include/generated/autoconf.h"
	got := compactKbuildObjectTreeObservationForTest(t, profile, command)
	want := []string{"include/generated/autoconf.h"}
	if !got.ObservesObjectTree || got.ObservesAll || !reflect.DeepEqual(got.References, want) {
		t.Fatalf("forwarded compiler observation = %#v, want exact references %q", got, want)
	}
	opaque := compactKbuildObjectTreeObservationForTest(
		t, profile,
		"sh ${tree:kernel}/scripts/compiler-wrapper.sh opaque-tool -I ${tree:prep}",
	)
	if !opaque.ObservesObjectTree || !opaque.ObservesAll || len(opaque.References) != 0 {
		t.Fatalf("forwarded opaque-tool observation = %#v, want conservative all-visible", opaque)
	}
}

func TestSourceScriptObjectTreeObservationComposesSourcedWrappers(t *testing.T) {
	profile := compactKbuildObjectTreeScriptProfileForTest(t, map[string]string{
		"scripts/parent.sh":   "#!/bin/sh\n. scripts/fragment.sh\n",
		"scripts/fragment.sh": "printf '%s\\n' \"${OBSERVED_ROOT}/arch/x86/include/generated\"\n",
	})
	got := compactKbuildObjectTreeObservationForTest(
		t, profile, `sh ${tree:kernel}/scripts/parent.sh`,
	)
	want := []string{"arch/x86/include/generated"}
	if !got.ObservesObjectTree || got.ObservesAll || !reflect.DeepEqual(got.References, want) {
		t.Fatalf("composed wrapper observation = %#v, want scoped references %q", got, want)
	}
}

func TestSourceScriptObjectTreeObservationComposesExtensionlessExecutableHelpers(t *testing.T) {
	profile := compactKbuildObjectTreeScriptProfileForTest(t, map[string]string{
		"scripts/parent-helper": "#!/bin/sh\nscripts/child-helper\n",
		"scripts/child-helper":  "#!/bin/sh\nprintf '%s\\n' \"${OBSERVED_ROOT}/include/generated/extensionless.h\"\n",
	})
	got := compactKbuildObjectTreeObservationForTest(
		t, profile, `${tree:kernel}/scripts/parent-helper`,
	)
	want := []string{"include/generated/extensionless.h"}
	if !got.ObservesObjectTree || got.ObservesAll || !reflect.DeepEqual(got.References, want) {
		t.Fatalf("extensionless helper observation=%#v, want scoped references %q", got, want)
	}
}

func TestSourceScriptBindsConfiguredBareProgramFromSourcedWrapper(t *testing.T) {
	profile := compactKbuildObjectTreeScriptProfileForTest(t, map[string]string{
		"scripts/parent.sh":   "#!/bin/sh\n. scripts/programs.sh\n",
		"scripts/programs.sh": "awk '{ print }' input\n",
	})
	invocation, matched, err := compactKbuildSourceScriptCommand(
		profile,
		compactKbuildRecipeCommand{
			program:   "sh",
			arguments: []string{"${tree:kernel}/scripts/parent.sh"},
		},
		map[string]string{"CONFIG_SHELL": "sh"},
		"target",
		testTargetActionRoles("awk", "cc"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("configured source-script invocation did not match")
	}
	if got, want := invocation.toolRoles, []string{"awk"}; !slices.Equal(got, want) {
		t.Fatalf("source-discovered auxiliary roles = %q, want %q", got, want)
	}
}
