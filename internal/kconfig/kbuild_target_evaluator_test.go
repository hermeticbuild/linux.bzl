package kconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestEvaluateCompactKbuildTargetUsesSourceDefinitionsAndTargetContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Makefile")
	content := `base = --target=$@ --first=$< --all=$^ --stem=$*
cmd_objtool = $(OBJTOOL) $(objtool-args) $@
rule_cc_o_c = $(CC) $(base) $(private-flag) -c -o $@ $< ; $(cmd_objtool)
$(obj)/%.o: private private-flag += --module=$(modname)
$(obj)/%.o: private objtool-args = --dynamic
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		Variables: map[string]string{
			"obj": "drivers/example", "CC": "/tool/cc", "OBJTOOL": "/tool/objtool",
		},
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("build:drivers/example", path, filepath.Dir(path), kb)
	if err != nil {
		t.Fatal(err)
	}
	values, err := EvaluateCompactKbuildTarget(
		profile,
		"drivers/example/foo.o",
		"foo",
		[]string{"drivers/example/foo.c", "include/generated/autoconf.h", "drivers/example/foo.c"},
		[]string{"FORCE"},
		map[string]string{"modname": "example"},
		"rule_cc_o_c", "cmd_objtool",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/tool/cc", "--target=drivers/example/foo.o", "--first=drivers/example/foo.c",
		"--all=drivers/example/foo.c include/generated/autoconf.h", "--stem=foo",
		"--module=example", "-c -o drivers/example/foo.o drivers/example/foo.c",
		"/tool/objtool --dynamic drivers/example/foo.o",
	} {
		if !strings.Contains(values["rule_cc_o_c"], want) {
			t.Fatalf("rule_cc_o_c = %q, want fragment %q", values["rule_cc_o_c"], want)
		}
	}
	if got, want := values["cmd_objtool"], "/tool/objtool --dynamic drivers/example/foo.o"; got != want {
		t.Fatalf("cmd_objtool = %q, want %q", got, want)
	}
}

func TestEvaluateCompactKbuildTargetNormalizesPrivateRootsForVirtualFiles(t *testing.T) {
	const releasePath = "include/config/kernel.release"
	publicRelease := "__LINUX_BZL_OBJECT_TREE__/" + releasePath
	publicLiteralRelease := "__LINUX_BZL_OBJECT_TREE__/include/config/__LINUX_BZL_OBJECT_TREE__.release"
	view := &testKbuildVirtualFileView{
		matches: map[string][]string{
			"__LINUX_BZL_OBJECT_TREE__/include/config/*.release": {publicRelease, publicLiteralRelease},
		},
		files: map[string]testKbuildVirtualFile{
			publicRelease: {content: "6.18.39-test\n", exact: true},
		},
	}
	path := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(path, []byte(`
KERNELRELEASE = $(file < $(objtree)/include/config/kernel.release)
release-files = $(wildcard $(objtree)/include/config/*.release)
output: FORCE
	@:
`), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKbuildFileTree(path, KbuildOptions{
		Variables:              map[string]string{"objtree": "__LINUX_BZL_OBJECT_TREE__"},
		VirtualFileView:        view,
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root:virtual-action-roots", path, filepath.Dir(path), parsed)
	if err != nil {
		t.Fatal(err)
	}
	injected := compactKbuildActionTreeInjections(map[string]string{
		"objtree": "__LINUX_BZL_OBJECT_TREE__",
	})
	values, err := EvaluateCompactKbuildTarget(
		profile, "output", "", []string{"FORCE"}, nil, injected,
		"KERNELRELEASE", "release-files",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["KERNELRELEASE"], "6.18.39-test"; got != want {
		t.Fatalf("KERNELRELEASE = %q, want %q", got, want)
	}
	if got, want := values["release-files"], strings.Join([]string{
		compactKbuildActionObjectTreeMarker + "/include/config/__LINUX_BZL_OBJECT_TREE__.release",
		compactKbuildActionObjectTreeMarker + "/" + releasePath,
	}, " "); got != want {
		t.Fatalf("release-files = %q, want private-rooted %q", got, want)
	}
	if got, want := view.readCalls, []string{publicRelease}; !slices.Equal(got, want) {
		t.Fatalf("virtual Read calls = %q, want public query %q", got, want)
	}
	if got, want := view.matchCalls, []string{"__LINUX_BZL_OBJECT_TREE__/include/config/*.release"}; !slices.Equal(got, want) {
		t.Fatalf("virtual Match calls = %q, want public query %q", got, want)
	}
}

func TestEvaluateCompactKbuildTargetRejectsPrivateVirtualWildcardResult(t *testing.T) {
	const publicPattern = "__LINUX_BZL_OBJECT_TREE__/include/config/*.release"
	view := &testKbuildVirtualFileView{
		matches: map[string][]string{
			publicPattern: {compactKbuildActionObjectTreeMarker + "/include/config/injected.release"},
		},
	}
	path := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(path, []byte(`
release-files = $(wildcard $(objtree)/include/config/*.release)
output: FORCE
	@:
`), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKbuildFileTree(path, KbuildOptions{
		Variables:              map[string]string{"objtree": "__LINUX_BZL_OBJECT_TREE__"},
		VirtualFileView:        view,
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root:private-virtual-wildcard", path, filepath.Dir(path), parsed)
	if err != nil {
		t.Fatal(err)
	}
	_, err = EvaluateCompactKbuildTarget(
		profile, "output", "", []string{"FORCE"}, nil,
		compactKbuildActionTreeInjections(map[string]string{
			"objtree": "__LINUX_BZL_OBJECT_TREE__",
		}),
		"release-files",
	)
	if err == nil || !strings.Contains(err.Error(), "reserved private action marker") {
		t.Fatalf("EvaluateCompactKbuildTarget() error = %v, Match calls = %q, want reserved provenance rejection", err, view.matchCalls)
	}
}

func TestEvaluateCompactKbuildTargetExpandsComputedNamesWithAutomaticAndEscapedDollars(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "root", "Makefile", "", `
ordinary_active = $(value_$*)
ordinary_escaped = $(value_$$*)
ordinary_mixed = $(value_$*_$$*)
ordinary_reverse = $(value_$$*_$*)
substitution_active = $(objects_$*:.o=.ko)
substitution_escaped = $(objects_$$*:.o=.ko)
substitution_mixed = $(objects_$*_$$*:%.o=built/%.ko)
`, map[string]string{
		"value_32":      "active",
		"value_$*":      "escaped",
		"value_32_$*":   "mixed",
		"value_$*_32":   "reverse",
		"objects_32":    "active.o plain",
		"objects_$*":    "escaped.o plain",
		"objects_32_$*": "mixed.o plain",
	})
	values, err := EvaluateCompactKbuildTarget(
		profile,
		"arch/arm64/include/generated/uapi/asm/syscall_table_32.h",
		"32",
		nil,
		nil,
		nil,
		"ordinary_active",
		"ordinary_escaped",
		"ordinary_mixed",
		"ordinary_reverse",
		"substitution_active",
		"substitution_escaped",
		"substitution_mixed",
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"ordinary_active":      "active",
		"ordinary_escaped":     "escaped",
		"ordinary_mixed":       "mixed",
		"ordinary_reverse":     "reverse",
		"substitution_active":  "active.ko plain",
		"substitution_escaped": "escaped.ko plain",
		"substitution_mixed":   "built/mixed.ko plain",
	} {
		if got := values[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestEvaluateCompactKbuildTargetExpandsUndefinedComputedSavedCommand(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:scripts/basic", "scripts/Makefile.build", "scripts/basic", `
empty :=
space := $(empty) $(empty)
space_escape := _-_SPACE_-_
cmd_link = ld -r -o $@ @$<
cmd-check = $(filter-out $(subst $(space),$(space_escape),$(strip $(savedcmd_$@))),$(subst $(space),$(space_escape),$(strip $(cmd_$1))))
selected = $(call cmd-check,link)
`, nil)
	values, err := EvaluateCompactKbuildTarget(
		profile,
		"scripts/basic/fixdep",
		"fixdep",
		[]string{"scripts/basic/fixdep.o"},
		nil,
		nil,
		"selected",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["selected"], "ld_-_SPACE_-_-r_-_SPACE_-_-o_-_SPACE_-_scripts/basic/fixdep_-_SPACE_-_@scripts/basic/fixdep.o"; got != want {
		t.Fatalf("selected = %q, want %q", got, want)
	}
}

func TestKbuildParserEvaluationClonesKeepSymbolicIdentityPrivate(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "root", "Makefile", "", "all:\n\t@true\n", nil)
	template := profile.evaluator.template
	template.symbolicVariables["FLAG"] = kbuildSymbolicVariableState{definedWhen: "symbolic"}

	first := cloneKbuildParserForEvaluation(template)
	second := cloneKbuildParserForEvaluation(template)
	first.setVariable("FLAG", kbuildVariable{value: "first"})
	if _, ok := template.symbolicVariables["FLAG"]; !ok {
		t.Fatal("evaluation clone mutated the template symbolic-variable registry")
	}
	if _, ok := second.symbolicVariables["FLAG"]; !ok {
		t.Fatal("evaluation clone mutated a sibling symbolic-variable registry")
	}

	target := cloneKbuildParserForTargetEvaluation(template, 1, false)
	target.setVariable("FLAG", kbuildVariable{value: "target"})
	if _, ok := template.symbolicVariables["FLAG"]; !ok {
		t.Fatal("target clone mutated the template symbolic-variable registry")
	}
}

func TestKbuildParserEvaluationClonesShareInitialVariablesCopyOnWrite(t *testing.T) {
	variables := make(map[string]string, 8192)
	for i := 0; i < 8192; i++ {
		variables[fmt.Sprintf("INITIAL_%04d", i)] = fmt.Sprintf("value-%04d", i)
	}
	template := newKbuildParser(variables, "")
	shared := template.initialVars

	first := cloneKbuildParserForEvaluation(template)
	second := cloneKbuildParserForEvaluation(template)
	target := cloneKbuildParserForTargetEvaluation(template, 2, false)
	for name, parser := range map[string]*kbuildParser{
		"first control clone":  first,
		"second control clone": second,
		"target clone":         target,
	} {
		if parser.initialVars != shared {
			t.Fatalf("%s copied the large immutable initial-variable layer", name)
		}
	}

	first.setVariable("INITIAL_0001", kbuildVariable{value: "first-local"})
	second.undefineVariable("INITIAL_0002")
	target.setVariable("INITIAL_0003", kbuildVariable{value: "target-local"})
	for name, want := range map[string]string{
		"INITIAL_0001": "value-0001",
		"INITIAL_0002": "value-0002",
		"INITIAL_0003": "value-0003",
	} {
		if got, ok := template.lookupVariable(name); !ok || got.value != want {
			t.Fatalf("template %s after sibling mutation = (%q, %t), want (%q, true)", name, got.value, ok, want)
		}
	}
	if got, ok := target.lookupVariable("INITIAL_0001"); !ok || got.value != "value-0001" {
		t.Fatalf("target INITIAL_0001 after control mutation = (%q, %t), want (%q, true)", got.value, ok, "value-0001")
	}
	if _, ok := first.lookupVariable("INITIAL_0002"); !ok {
		t.Fatal("first control clone inherited sibling undefine")
	}

	first.applyEnvironmentVariables(map[string]string{
		"INITIAL_0004": "first-environment",
		"FIRST_ONLY":   "present",
	})
	if first.initialVars == shared || first.initialVars.parent != shared {
		t.Fatal("environment mutation did not create a private overlay over the shared initial-variable layer")
	}
	if got, ok := first.lookupVariable("INITIAL_0004"); !ok || got.value != "first-environment" {
		t.Fatalf("first INITIAL_0004 = (%q, %t), want (%q, true)", got.value, ok, "first-environment")
	}
	for name, parser := range map[string]*kbuildParser{
		"template":             template,
		"second control clone": second,
		"target clone":         target,
	} {
		if got, ok := parser.lookupVariable("INITIAL_0004"); !ok || got.value != "value-0004" {
			t.Fatalf("%s INITIAL_0004 after sibling environment mutation = (%q, %t), want (%q, true)", name, got.value, ok, "value-0004")
		}
		if got, ok := parser.lookupVariable("FIRST_ONLY"); ok {
			t.Fatalf("%s inherited sibling-only environment value %q", name, got.value)
		}
		if parser.initialVars != shared {
			t.Fatalf("%s detached its initial-variable layer after a sibling mutation", name)
		}
	}
}

func TestEvaluateCompactKbuildTargetRejectsSerializedProfileWithoutEvaluator(t *testing.T) {
	_, err := EvaluateCompactKbuildTarget(CompactKbuildProfile{Name: "serialized"}, "foo.o", "foo", nil, nil, nil, "cmd_cc_o_c")
	if err == nil || !strings.Contains(err.Error(), "no source-derived target evaluator") {
		t.Fatalf("EvaluateCompactKbuildTarget() error = %v", err)
	}
}

func TestEvaluateCompactKbuildTargetEnvironmentUsesParsedAndTargetExports(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:drivers/demo", "scripts/Makefile.build", "drivers/demo", `
export GLOBAL = base-$(SELECTED)
LOCAL = hidden
drivers/demo/%.o: export TARGET_ENV = target-$*
drivers/demo/%.o: private PRIVATE_ENV = not-exported
drivers/demo/%.o: drivers/demo/%.c
`, map[string]string{"SELECTED": "real"})

	environment, err := EvaluateCompactKbuildTargetEnvironment(
		profile,
		"drivers/demo/example.o",
		"example",
		[]string{"drivers/demo/example.c"},
		nil,
		map[string]string{"SELECTED": "configured"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := environment["GLOBAL"], "base-configured"; got != want {
		t.Fatalf("GLOBAL=%q, want %q", got, want)
	}
	if got, want := environment["TARGET_ENV"], "target-example"; got != want {
		t.Fatalf("TARGET_ENV=%q, want %q", got, want)
	}
	for _, absent := range []string{"LOCAL", "PRIVATE_ENV"} {
		if _, ok := environment[absent]; ok {
			t.Fatalf("environment unexpectedly exports %s: %#v", absent, environment)
		}
	}
	if profile.evaluator == nil || profile.evaluator.template == nil || len(profile.evaluator.template.kb.Rules) != 0 {
		t.Fatalf("profile evaluator retains the projected parsed rule graph: %#v", profile.evaluator)
	}
}

func TestEvaluateCompactKbuildTargetEnvironmentSymbolicPreservesProbeDependencies(t *testing.T) {
	token := "LINUX_BZL_PROBE_" + strings.Repeat("a", 64)
	path := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(path, []byte("export KBUILD_CPPFLAGS = -DBASE "+token+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKbuildFileTree(path, KbuildOptions{
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
		SkipExportedVariables:  true,
		ResolveSymbolic: func(value string) (string, error) {
			return strings.ReplaceAll(value, token, "-fmeasured"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("driver:test", path, filepath.Dir(path), parsed)
	if err != nil {
		t.Fatal(err)
	}
	symbolic, err := EvaluateCompactKbuildTargetEnvironmentSymbolic(profile, "all", "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := symbolic["KBUILD_CPPFLAGS"], "-DBASE "+token; got != want {
		t.Fatalf("symbolic KBUILD_CPPFLAGS = %q, want %q", got, want)
	}
	concrete, err := EvaluateCompactKbuildTargetEnvironment(profile, "all", "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := concrete["KBUILD_CPPFLAGS"], "-DBASE -fmeasured"; got != want {
		t.Fatalf("concrete KBUILD_CPPFLAGS = %q, want %q", got, want)
	}
}

func TestCompactKbuildTargetParserDoesNotCloneUnusedExports(t *testing.T) {
	template := newKbuildParser(map[string]string{"VALUE": "selected"}, t.TempDir())
	for index := 0; index < 4096; index++ {
		template.exported[fmt.Sprintf("EXPORTED_%04d", index)] = true
	}
	profile := CompactKbuildProfile{
		Name: "export-growth",
		TargetVariables: []KbuildTargetVariable{{
			Targets: []string{"%.o"}, Variable: "LOCAL", Operator: "=", Value: "target",
			Modifiers: []string{"export"},
		}},
		evaluator: newKbuildTargetEvaluator(template),
	}

	parser, cleanup, err := compactKbuildTargetParserWithExports(
		profile, "selected.o", "selected.o", "selected", []string{"selected.c"}, nil, nil, true, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got := len(parser.exported); got != 0 {
		t.Fatalf("non-environment parser retained %d exported variables, want 0", got)
	}
	if got := len(template.exported); got != 4096 {
		t.Fatalf("template export set was mutated: got %d entries, want 4096", got)
	}
	if variable, ok := parser.lookupVariable("LOCAL"); !ok || variable.value != "target" {
		t.Fatalf("target variable = %#v, %t; want target", variable, ok)
	}
}

func TestEvaluateCompactKbuildTextExpandsSelectedRecursiveMakeRecipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(path, []byte(`
build = -f $(srctree)/scripts/Makefile.build obj
tools/%: FORCE
	$(MAKE) -C $(srctree)/tools $*
scripts: FORCE
	$(MAKE) $(build)=scripts scripts/unifdef
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		Variables: map[string]string{
			"MAKE": "make", "srctree": "__LINUX_BZL_SOURCE_TREE__",
		},
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root:selected", path, filepath.Dir(path), kb)
	if err != nil {
		t.Fatal(err)
	}
	got, err := EvaluateCompactKbuildText(profile, "tools/objtool", "objtool", []string{"FORCE"}, nil, nil, profile.Rules[0].Recipe[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := "make -C __LINUX_BZL_SOURCE_TREE__/tools objtool"; got != want {
		t.Fatalf("tool recipe = %q, want %q", got, want)
	}
	got, err = EvaluateCompactKbuildText(profile, "scripts", "", []string{"FORCE"}, nil, nil, profile.Rules[1].Recipe[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := "make -f __LINUX_BZL_SOURCE_TREE__/scripts/Makefile.build obj=scripts scripts/unifdef"; got != want {
		t.Fatalf("build recipe = %q, want %q", got, want)
	}
}

func TestEvaluateCompactKbuildTextActionRolesPreservesSourceAssignments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(path, []byte(`
HOST_OVERRIDES := CC="$(HOSTCC)" LD="$(HOSTLD)"
FORWARDED = $(HOST_OVERRIDES) AR="$(HOSTAR)"
$(eval EVAL_TOOL := $(HOSTCC))
run: export CHILD_TOOLS = $(FORWARDED)
run: FORCE
	$(MAKE) $(FORWARDED) CHECK="$(EVAL_TOOL)" child
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		Variables: map[string]string{"MAKE": CompactKbuildRecursiveMakeProvenanceToken},
		CommandLineVariables: map[string]string{
			"CC":     KbuildActionRoleToken("target", "cc"),
			"HOSTCC": KbuildActionRoleToken("host", "cc"),
			"HOSTLD": KbuildActionRoleToken("host", "ld"),
			"HOSTAR": KbuildActionRoleToken("host", "ar"),
		},
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root:selected", path, filepath.Dir(path), kb)
	if err != nil {
		t.Fatal(err)
	}
	text, refs, err := EvaluateCompactKbuildTextActionRolesForAutomaticTarget(
		profile, "run", "run", "", []string{"FORCE"}, nil, nil, profile.Rules[0].Recipe[0],
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, KbuildActionRoleToken("target", "cc")) {
		t.Fatalf("recursive Make text incorrectly selected target compiler: %q", text)
	}
	want := []KbuildActionRoleRef{{Scope: "host", Role: "ar"}, {Scope: "host", Role: "cc"}, {Scope: "host", Role: "ld"}}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("action refs = %#v, want %#v; text=%q", refs, want, text)
	}
	environment, err := EvaluateCompactKbuildTargetEnvironment(profile, "run", "", []string{"FORCE"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	environmentRefs, err := KbuildActionRoleRefs(environment["CHILD_TOOLS"])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(environmentRefs, want) {
		t.Fatalf("exported action refs = %#v, want %#v", environmentRefs, want)
	}
}

func TestEvaluateCompactKbuildTextActionRolesIncludesSelectedCommandTemplate(t *testing.T) {
	for _, test := range []struct {
		name        string
		template    string
		want        []KbuildActionRoleRef
		wantPrimary []KbuildActionRoleRef
	}{
		{name: "host", template: "$(HOSTCC) -o $@ $<", want: []KbuildActionRoleRef{{Scope: "host", Role: "cc"}}, wantPrimary: []KbuildActionRoleRef{{Scope: "host", Role: "cc"}}},
		{name: "target", template: "$(CC) -c -o $@ $<", want: []KbuildActionRoleRef{{Scope: "target", Role: "cc"}}, wantPrimary: []KbuildActionRoleRef{{Scope: "target", Role: "cc"}}},
		{name: "mixed", template: "$(HOSTCC) $(CC) -o $@ $<", want: []KbuildActionRoleRef{{Scope: "host", Role: "cc"}, {Scope: "target", Role: "cc"}}, wantPrimary: []KbuildActionRoleRef{{Scope: "host", Role: "cc"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "Makefile")
			content := fmt.Sprintf(`
if_changed = $(if $(suppress-command),,$(cmd_$(1)))
suppress-command := y
run: private chosen = selected
run: private nested = %s
run: private cmd_selected = $(nested)
run: input.c
	$(call if_changed,$(chosen))
`, test.template)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			kb, err := ParseKbuildFileTree(path, KbuildOptions{
				CommandLineVariables: map[string]string{
					"CC": KbuildActionRoleToken("target", "cc"), "HOSTCC": KbuildActionRoleToken("host", "cc"),
				},
				MakeVariablesComplete: true, CaptureTargetEvaluator: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			profile, err := NewCompactKbuildProfile("root:selected-command", path, filepath.Dir(path), kb)
			if err != nil {
				t.Fatal(err)
			}
			text, wrapperRefs, err := EvaluateCompactKbuildTextActionRolesForAutomaticTarget(
				profile, "run", "run", "", []string{"input.c"}, nil, nil, profile.Rules[0].Recipe[0],
			)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(text) != "" {
				t.Fatalf("suppressed wrapper text = %q, want empty", text)
			}
			if len(wrapperRefs) != 0 {
				t.Fatalf("suppressed wrapper refs = %#v, want none before exact command selection", wrapperRefs)
			}
			effects, selected, err := EvaluateCompactKbuildSelectedTargetEffects(profile, "run")
			if err != nil {
				t.Fatal(err)
			}
			if !selected {
				t.Fatal("selected command target was not materialized")
			}
			if !reflect.DeepEqual(effects.ActionRoles, test.want) {
				t.Fatalf("selected command refs = %#v, want %#v", effects.ActionRoles, test.want)
			}
			if !reflect.DeepEqual(effects.PrimaryActionRoles, test.wantPrimary) {
				t.Fatalf("selected primary command refs = %#v, want %#v", effects.PrimaryActionRoles, test.wantPrimary)
			}
		})
	}
}

func TestEvaluateCompactKbuildSelectedTargetEffectsKeepsDeferredQueryRolesSeparate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(path, []byte(`
if_changed = $(cmd_$(1))
query-result = $(shell $(QUERY_TOOL) input.txt)
cmd_emit = printf '%s' $(query-result) > $@
result: input.txt FORCE
	$(call if_changed,emit)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKbuildFileTree(path, KbuildOptions{
		CommandLineVariables: map[string]string{
			"QUERY_TOOL": KbuildActionRoleToken("target", "awk"),
		},
		MakeVariablesComplete: true, CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root:deferred-role", path, filepath.Dir(path), parsed)
	if err != nil {
		t.Fatal(err)
	}
	effects, selected, err := EvaluateCompactKbuildSelectedTargetEffects(profile, "result")
	if err != nil {
		t.Fatal(err)
	}
	if !selected {
		t.Fatal("deferred-query target was not selected")
	}
	if len(effects.ActionRoles) != 0 {
		t.Fatalf("selected consumer refs = %#v, want no deferred-query roles", effects.ActionRoles)
	}
	evaluation, err := EvaluateSelectedKbuildControlEffects(profile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(evaluation.Queries), 1; got != want {
		t.Fatalf("deferred queries = %#v, want %d", evaluation.Queries, want)
	}
	want := []KbuildActionRoleRef{{Scope: "target", Role: "awk"}}
	if got := evaluation.Queries[0].ActionRoles; !reflect.DeepEqual(got, want) {
		t.Fatalf("deferred-query refs = %#v, want %#v", got, want)
	}
}

func TestEvaluateCompactKbuildSelectedTargetEffectsUsesMergedInstantiatedPrerequisites(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "Makefile")
	if err := os.WriteFile(path, []byte(`
if_changed = $(cmd_$(1))
nested-host = $(HOSTCC)
nested-target = $(CC)
host.o: merged.c
%.o: private chosen = compile
%.o: private cmd_compile = $(if $(filter host,$(target-stem)),$(if $(filter host.c,$<),$(if $(filter merged.c,$^),$(nested-host),$(nested-target)),$(nested-target)),$(nested-target)) -c -o $@ $<
%.o: %.c
	$(call if_changed,$(chosen))
`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"host.c", "merged.c"} {
		if err := os.WriteFile(filepath.Join(directory, source), []byte("source\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		CommandLineVariables: map[string]string{
			"CC": KbuildActionRoleToken("target", "cc"), "HOSTCC": KbuildActionRoleToken("host", "cc"),
		},
		MakeVariablesComplete: true, CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root:merged-context", path, directory, kb)
	if err != nil {
		t.Fatal(err)
	}
	effects, selected, err := EvaluateCompactKbuildSelectedTargetEffects(profile, "host.o")
	if err != nil {
		t.Fatal(err)
	}
	if !selected {
		t.Fatal("merged implicit rule target was not materialized")
	}
	// Selection uses the same tree-rooted automatic-variable context as final
	// lowering. The literal host.c filter does not match that rooted $<, so the
	// source expression selects its target-scoped fallback.
	want := []KbuildActionRoleRef{{Scope: "target", Role: "cc"}}
	if !reflect.DeepEqual(effects.ActionRoles, want) {
		t.Fatalf("merged instantiated command refs = %#v, want %#v", effects.ActionRoles, want)
	}
}

func TestEvaluateCompactKbuildSelectedTargetEffectsFallsBackForLegacyBackticks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Makefile")
	content := strings.Join([]string{
		"if_changed = $(cmd_$(1))",
		"export TOOL_FLAGS := $(HOSTCC)",
		"cmd_emit = { printf '%s\\n' `printf '%s' \"$$TOOL_FLAGS\"`; } > $@",
		"result: FORCE",
		"\t$(call if_changed,emit)",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKbuildFileTree(path, KbuildOptions{
		CommandLineVariables: map[string]string{
			"HOSTCC": KbuildActionRoleToken("host", "cc"),
		},
		MakeVariablesComplete: true, CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root:legacy-backtick", path, filepath.Dir(path), parsed)
	if err != nil {
		t.Fatal(err)
	}
	effects, selected, err := EvaluateCompactKbuildSelectedTargetEffects(profile, "result")
	if err != nil {
		t.Fatal(err)
	}
	if !selected {
		t.Fatal("legacy-backtick target was not selected")
	}
	want := []KbuildActionRoleRef{{Scope: "host", Role: "cc"}}
	if got := effects.ActionRoles; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy-backtick exported environment refs = %#v, want %#v", got, want)
	}
}

func TestKbuildActionRoleAutoScopeResolvesAgainstSelectedManifest(t *testing.T) {
	token := KbuildActionRoleToken(KbuildActionRoleAutoScope, "awk")
	refs, err := KbuildActionRoleRefs("PATH=" + token)
	if err != nil {
		t.Fatal(err)
	}
	wantRefs := []KbuildActionRoleRef{{Scope: KbuildActionRoleAutoScope, Role: "awk"}}
	if !reflect.DeepEqual(refs, wantRefs) {
		t.Fatalf("scope-neutral refs = %#v, want %#v", refs, wantRefs)
	}
	configured := testScopedActionRoles("awk")
	for _, scope := range []string{"target", "host"} {
		rewritten, roles, err := rewriteKbuildActionRoleRefs("PATH="+token, scope, configured, true)
		if err != nil {
			t.Fatalf("resolve scope-neutral AWK for %s: %v", scope, err)
		}
		if got, want := rewritten, "PATH=${tool:awk}"; got != want {
			t.Fatalf("%s rewrite = %q, want %q", scope, got, want)
		}
		if !reflect.DeepEqual(roles, []string{"awk"}) {
			t.Fatalf("%s roles = %q, want awk", scope, roles)
		}
	}
	_, _, err = rewriteKbuildActionRoleRefs(token, "host", testTargetActionRoles("awk"), true)
	if err == nil || !strings.Contains(err.Error(), "unconfigured host action role") {
		t.Fatalf("missing host utility role error = %v", err)
	}
}

func TestEvaluateCompactKbuildSelectedTargetEffectsProjectsSourceScriptEnvironment(t *testing.T) {
	for name, test := range map[string]struct {
		script string
		want   []KbuildActionRoleRef
	}{
		"target": {
			script: `"$CC" -o result.o input.c`,
			want:   []KbuildActionRoleRef{{Scope: "target", Role: "cc"}},
		},
		"host": {
			script: `"$HOSTCC" -o helper helper.c`,
			want:   []KbuildActionRoleRef{{Scope: "host", Role: "cc"}},
		},
		"host generator": {
			script: `"$YACC" -o parser.c parser.y`,
			want:   []KbuildActionRoleRef{{Scope: "host", Role: "bison"}},
		},
		"mixed": {
			script: `"$HOSTCC" -o helper helper.c; "$CC" -o result.o input.c`,
			want: []KbuildActionRoleRef{
				{Scope: "host", Role: "cc"},
				{Scope: "target", Role: "cc"},
			},
		},
		"dynamic": {
			script: `env >/dev/null`,
			want: []KbuildActionRoleRef{
				{Scope: "host", Role: "bison"},
				{Scope: "host", Role: "cc"},
				{Scope: "target", Role: "cc"},
			},
		},
		"unused exports": {
			// orc_hash.sh-style checksum/filter logic does not consume the
			// root Makefile's exported compiler or parser-generator roles.
			script: `printf '%s\n' payload | awk '{ print $1 }' | sha1sum | cut -c-16`,
			want:   []KbuildActionRoleRef{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			mustWriteSource(t, root, "scripts/selected.sh", "#!/bin/sh\n"+test.script+"\n")
			makefile := filepath.Join(root, "Makefile")
			mustWriteSource(t, root, "Makefile", `
CONFIG_SHELL := sh
export CC HOSTCC YACC
export ROLE_FREE := preserved
if_changed = $(cmd_$(1))
cmd_selected = $(CONFIG_SHELL) $(srctree)/scripts/selected.sh > $@
result: scripts/selected.sh FORCE
	$(call if_changed,selected)
`)
			kb, err := ParseKbuildFileTree(makefile, KbuildOptions{
				RootDir: root,
				Variables: map[string]string{
					"srctree": root,
				},
				CommandLineVariables: map[string]string{
					"CC": KbuildActionRoleToken("target", "cc"), "HOSTCC": KbuildActionRoleToken("host", "cc"),
					"YACC": KbuildActionRoleToken("host", "bison"),
				},
				SourceRoots:             map[string]string{"__LINUX_BZL_SOURCE_TREE__": root},
				ConfigVariablesComplete: true, MakeVariablesComplete: true, CaptureTargetEvaluator: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			profile, err := NewCompactKbuildProfile("root:script-env", makefile, root, kb)
			if err != nil {
				t.Fatal(err)
			}
			effects, selected, err := EvaluateCompactKbuildSelectedTargetEffects(profile, "result")
			if err != nil {
				t.Fatal(err)
			}
			if !selected {
				t.Fatal("source-script target was not selected")
			}
			if !reflect.DeepEqual(effects.ActionRoles, test.want) {
				t.Fatalf("source-script action refs = %#v, want %#v", effects.ActionRoles, test.want)
			}
		})
	}
}

func TestEvaluateCompactKbuildTextDoesNotExecuteRecipeShell(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(path, []byte(`generated: FORCE
	$(shell mkdir -p $(dir $@))
`), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		Shell: func(command string) (string, error) {
			called = true
			return "", fmt.Errorf("unexpected shell command %q", command)
		},
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root:selected", path, filepath.Dir(path), kb)
	if err != nil {
		t.Fatal(err)
	}
	_, err = EvaluateCompactKbuildText(profile, "generated", "", []string{"FORCE"}, nil, nil, profile.Rules[0].Recipe[0])
	if err == nil || !strings.Contains(err.Error(), "requires a hermetic evaluator") {
		t.Fatalf("EvaluateCompactKbuildText() error = %v, want disabled shell evaluator", err)
	}
	if called {
		t.Fatal("EvaluateCompactKbuildText() executed the parser's shell callback")
	}
}

func TestEvaluateCompactKbuildTextRetainsHermeticSourceShell(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "Makefile")
	if err := os.WriteFile(path, []byte(`
generated: private source_flags = $(shell grep -Ev '^#|^$$' source.parameters)
generated: FORCE
	@true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	generalCalled := false
	sourceCalled := false
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		RootDir:    root,
		WorkingDir: root,
		Shell: func(command string) (string, error) {
			generalCalled = true
			return "", &LinuxProbeUnsupportedCommandError{Command: command}
		},
		SourceShell: func(command, workingDirectory string) (string, error) {
			sourceCalled = true
			if command != "grep -Ev '^#|^$' source.parameters" || workingDirectory != root {
				return "", fmt.Errorf("unexpected source query %q in %q", command, workingDirectory)
			}
			return "--first\n--second\n", nil
		},
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	generalCalled = false
	sourceCalled = false
	profile, err := NewCompactKbuildProfile("root:source-query", path, root, kb)
	if err != nil {
		t.Fatal(err)
	}
	value, err := EvaluateCompactKbuildText(
		profile, "generated", "", []string{"FORCE"}, nil, nil, "$(source_flags)",
	)
	if err != nil {
		t.Fatal(err)
	}
	if value != "--first --second" || !sourceCalled || generalCalled {
		t.Fatalf(
			"source query = %q, sourceCalled=%v, generalCalled=%v; want normalized hermetic source output",
			value, sourceCalled, generalCalled,
		)
	}
}

func TestTargetContextDoesNotExpandUnrequestedRecursiveSourceQuery(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "Makefile")
	if err := os.WriteFile(path, []byte(`
generated: private source_flags = $(shell grep -Ev '^#|^$$' $(src)/source.parameters)
generated: FORCE
	@true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	var commands []string
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		RootDir: root,
		Variables: map[string]string{
			"src": sourceShellQuerySourceTreeMarker,
		},
		Shell: func(command string) (string, error) {
			return "", &LinuxProbeUnsupportedCommandError{Command: command}
		},
		SourceShell: func(command, _ string) (string, error) {
			commands = append(commands, command)
			return "--source-flag\n", nil
		},
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 {
		t.Fatalf("source commands while parsing recursive target assignment = %q, want none", commands)
	}
	profile, err := NewCompactKbuildProfile("root:source-query", path, root, kb)
	if err != nil {
		t.Fatal(err)
	}
	values, err := EvaluateCompactKbuildTarget(
		profile, "generated", "", []string{"FORCE"}, nil,
		map[string]string{"src": "logical-source-directory"},
		"unrelated",
	)
	if err != nil {
		t.Fatal(err)
	}
	if values["unrelated"] != "" || len(commands) != 0 {
		t.Fatalf("unrelated target value = %q, source commands = %q; want no recursive RHS expansion", values["unrelated"], commands)
	}
	values, err = EvaluateCompactKbuildTarget(
		profile, "generated", "", []string{"FORCE"}, nil,
		map[string]string{"src": sourceShellQuerySourceTreeMarker},
		"source_flags",
	)
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := "grep -Ev '^#|^$' " + sourceShellQuerySourceTreeMarker + "/source.parameters"
	if values["source_flags"] != "--source-flag" || !slices.Equal(commands, []string{wantCommand}) {
		t.Fatalf("source flags = %q, commands = %q; want one rooted source query %q", values["source_flags"], commands, wantCommand)
	}
}

func TestEvaluateCompactKbuildTargetExpandsUndefinedVariableToEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Makefile")
	if err := os.WriteFile(path, []byte("defined = value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := ParseKbuildFileTree(path, KbuildOptions{
		MakeVariablesComplete:  true,
		CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("undefined", path, filepath.Dir(path), kb)
	if err != nil {
		t.Fatal(err)
	}
	values, err := EvaluateCompactKbuildTarget(profile, "out", "out", nil, nil, nil, "defined", "optional")
	if err != nil {
		t.Fatal(err)
	}
	if got := values["defined"]; got != "value" {
		t.Fatalf("defined = %q, want value", got)
	}
	if got := values["optional"]; got != "" {
		t.Fatalf("optional = %q, want empty", got)
	}
}

func TestEvaluateCompactKbuildTargetUsesSelectedStemWithRenderedObjectTree(t *testing.T) {
	const target = "arch/arm64/kernel/vmlinux.lds"
	profile := mustCompactKbuildProfileForTest(t, "build:arch/arm64/kernel", "scripts/Makefile.build", "arch/arm64/kernel", `
target-stem = $(basename $(patsubst $(obj)/%,%,$@))
_cpp_flags = $(KBUILD_CPPFLAGS) $(CPPFLAGS_$(target-stem).lds)
cmd_cpp_lds_S = $(CC) $(_cpp_flags) -o $@ $<
arch/arm64/kernel/%.lds: arch/arm64/kernel/%.lds.S
`, map[string]string{
		"CC": "/selected/cc", "obj": "arch/arm64/kernel", "KBUILD_CPPFLAGS": "-D__KERNEL__",
		"CPPFLAGS_vmlinux.lds": "-P -Uarm64 -D__ASSEMBLY__ -DLINKER_SCRIPT",
	})
	values, err := EvaluateCompactKbuildTarget(
		profile, target, "vmlinux", []string{target + ".S"}, nil, map[string]string{
			"obj": "${tree:prep}/arch/arm64/kernel", "target-stem": "vmlinux",
		},
		"target-stem", "cmd_cpp_lds_S",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["target-stem"], "vmlinux"; got != want {
		t.Fatalf("target-stem=%q, want %q", got, want)
	}
	if got := values["cmd_cpp_lds_S"]; strings.Contains(got, "$(") ||
		!strings.Contains(got, "-P -Uarm64 -D__ASSEMBLY__ -DLINKER_SCRIPT") {
		t.Fatalf("cmd_cpp_lds_S=%q, want fully expanded target-specific flags", got)
	}
}

func TestEvaluateCompactKbuildTargetDerivesMultiObjectModuleName(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:arch/x86/events/intel", "scripts/Makefile.build", "arch/x86/events/intel", `
empty :=
space := $(empty) $(empty)
comma := ,
suffix-search = $(strip $(foreach s,$3,$($(1:%$(strip $2)=%$s))))
multi-obj-ym := intel-uncore.o
intel-uncore-objs := uncore.o uncore_nhmex.o uncore_snb.o
modname-multi = $(sort $(foreach m,$(multi-obj-ym),\
	$(if $(filter $*.o, $(call suffix-search, $m, .o, -objs -y -m)),$(m:.o=))))
basetarget = $(basename $(notdir $@))
__modname = $(or $(modname-multi),$(basetarget))
modname = $(subst $(space),:,$(__modname))
modfile = $(addprefix $(obj)/,$(__modname))
name-fix-token = $(subst $(comma),_,$(subst -,_,$1))
name-fix = "$(call name-fix-token,$1)"
stringify = "$1"
modname_flags = -DKBUILD_MODNAME=$(call name-fix,$(modname)) -D__KBUILD_MODNAME=kmod_$(call name-fix-token,$(modname))
modfile_flags = -DKBUILD_MODFILE=$(call stringify,$(modfile))
`, map[string]string{"obj": "arch/x86/events/intel"})

	values, err := EvaluateCompactKbuildTarget(
		profile,
		"arch/x86/events/intel/uncore.o",
		"uncore",
		[]string{"arch/x86/events/intel/uncore.c"},
		nil,
		nil,
		"modname", "modfile", "modname_flags", "modfile_flags",
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"modname":       "intel-uncore",
		"modfile":       "arch/x86/events/intel/intel-uncore",
		"modname_flags": `-DKBUILD_MODNAME="intel_uncore" -D__KBUILD_MODNAME=kmod_intel_uncore`,
		"modfile_flags": `-DKBUILD_MODFILE="arch/x86/events/intel/intel-uncore"`,
	} {
		if got := values[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestEvaluateCompactKbuildTargetKeepsSourceDerivedExplicitTargetStem(t *testing.T) {
	const target = "arch/x86/tools/relocs"
	profile := mustCompactKbuildProfileForTest(t, "host:relocs", "scripts/Makefile.host", "arch/x86/tools", `
target-stem = $(basename $(patsubst $(obj)/%,%,$@))
relocs-objs := relocs_32.o relocs_64.o relocs_common.o
cmd_host-cmulti = $(HOSTCC) -o $@ $(addprefix $(obj)/,$($(target-stem)-objs))
arch/x86/tools/relocs: FORCE
	$(call if_changed,host-cmulti)
`, map[string]string{
		"HOSTCC": KbuildActionRoleToken("host", "cc"),
		"obj":    "arch/x86/tools",
	})
	injected, err := compactKbuildSourceScriptInjectionsForTarget(
		profile,
		target,
		"",
		[]string{"FORCE"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	values, err := EvaluateCompactKbuildTarget(
		profile,
		target,
		"",
		[]string{"FORCE"},
		nil,
		injected,
		"target-stem",
		"cmd_host-cmulti",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := values["target-stem"], "relocs"; got != want {
		t.Fatalf("target-stem = %q, want source-derived %q", got, want)
	}
	for _, object := range []string{"relocs_32.o", "relocs_64.o", "relocs_common.o"} {
		if !strings.Contains(values["cmd_host-cmulti"], "__LINUX_BZL_OBJECT_TREE__/arch/x86/tools/"+object) {
			t.Errorf("cmd_host-cmulti = %q, want %s", values["cmd_host-cmulti"], object)
		}
	}
}

func TestCompactKbuildTargetEvaluationInjectionsKeepParentInvocationDirectoryForChildGoal(t *testing.T) {
	const (
		directory = "arch/x86/boot"
		target    = directory + "/compressed/vmlinux"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
build = -f $(srctree)/scripts/Makefile.build obj
$(obj)/compressed/vmlinux: FORCE
	$(MAKE) $(build)=$(obj)/compressed $@
`, map[string]string{
		"MAKE": CompactKbuildRecursiveMakeProvenanceToken,
	})
	injected, err := compactKbuildSourceScriptInjectionsForTarget(
		profile, target, "", []string{"FORCE"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := injected["obj"], "__LINUX_BZL_OBJECT_TREE__/"+directory; got != want {
		t.Fatalf("obj injection = %q, want parent invocation directory %q", got, want)
	}
	if got, want := injected["src"], "__LINUX_BZL_SOURCE_TREE__/"+directory; got != want {
		t.Fatalf("src injection = %q, want parent invocation directory %q", got, want)
	}
	command, err := EvaluateCompactKbuildTextSymbolicForAutomaticTarget(
		profile, target, target, "", []string{"FORCE"}, nil, injected,
		`$(MAKE) $(build)=$(obj)/compressed $@`,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := CompactKbuildRecursiveMakeProvenanceToken + " -f __LINUX_BZL_SOURCE_TREE__/scripts/Makefile.build obj=__LINUX_BZL_OBJECT_TREE__/arch/x86/boot/compressed " + target
	if command != want {
		t.Fatalf("recursive command = %q, want %q", command, want)
	}
}

func TestCompactKbuildTargetEvaluationInjectionsUseNestedExternalSourceOverlay(t *testing.T) {
	const directory = "external/module"
	profile := mustCompactKbuildProfileForTest(
		t, "external", "scripts/Makefile.build", directory,
		"target-stem = $(notdir $(basename $@))\n", nil,
	)
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":                      t.TempDir(),
		"__LINUX_BZL_SOURCE_TREE__/external":             t.TempDir(),
		"__LINUX_BZL_SOURCE_TREE__/external/module":      t.TempDir(),
		"__LINUX_BZL_SOURCE_TREE__/external/module/peer": t.TempDir(),
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	injected, err := compactKbuildSourceScriptInjectionsForTarget(
		profile, directory+"/module.o", "module", nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"abs_output":  "__LINUX_BZL_OBJECT_TREE__/" + directory,
		"abs_srctree": "__LINUX_BZL_SOURCE_TREE__",
		"obj":         "__LINUX_BZL_OBJECT_TREE__/" + directory,
		"objtree":     "__LINUX_BZL_OBJECT_TREE__",
		"src":         "__LINUX_BZL_OBJECT_TREE__/" + directory,
		"srcroot":     "__LINUX_BZL_OBJECT_TREE__/" + directory,
		"srctree":     "__LINUX_BZL_SOURCE_TREE__",
	}
	for name, value := range want {
		if got := injected[name]; got != value {
			t.Errorf("%s injection = %q, want %q; all injections=%#v", name, got, value, injected)
		}
	}
}

func TestCompactKbuildTargetEvaluationInjectionsDoNotInferOverlayFromObjectInvocation(t *testing.T) {
	const directory = "drivers/example"
	profile := mustCompactKbuildProfileForTest(
		t, "internal", "scripts/Makefile.build", directory,
		"target-stem = $(notdir $(basename $@))\n", nil,
	)
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__": t.TempDir(),
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	injected, err := compactKbuildSourceScriptInjectionsForTarget(
		profile, directory+"/example.o", "example", nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := injected["src"], "__LINUX_BZL_SOURCE_TREE__/"+directory; got != want {
		t.Fatalf("internal src injection = %q, want %q", got, want)
	}
	if got, want := injected["srcroot"], "__LINUX_BZL_SOURCE_TREE__"; got != want {
		t.Fatalf("internal srcroot injection = %q, want %q", got, want)
	}
	if got, want := injected["abs_output"], "__LINUX_BZL_OBJECT_TREE__"; got != want {
		t.Fatalf("internal abs_output injection = %q, want %q", got, want)
	}
}

func TestCompactKbuildTargetEvaluationInjectionsRequireObjectOverlayInvocation(t *testing.T) {
	const directory = "external/module"
	profile := mustCompactKbuildProfileForTest(
		t, "external-source", "scripts/Makefile.build", directory,
		"target-stem = $(notdir $(basename $@))\n", nil,
	)
	profile.evaluator.template.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":              t.TempDir(),
		"__LINUX_BZL_SOURCE_TREE__/" + directory: t.TempDir(),
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	injected, err := compactKbuildSourceScriptInjectionsForTarget(
		profile, directory+"/module.o", "module", nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := injected["src"], "__LINUX_BZL_SOURCE_TREE__/"+directory; got != want {
		t.Fatalf("source-tree invocation src injection = %q, want %q", got, want)
	}
	if got, want := injected["srcroot"], "__LINUX_BZL_SOURCE_TREE__"; got != want {
		t.Fatalf("source-tree invocation srcroot injection = %q, want %q", got, want)
	}
}

func TestCompactKbuildTargetEvaluationProjectsObjOnlyForComputedVariableNames(t *testing.T) {
	const (
		directory = "resolve_btfids"
		target    = "tools/bpf/resolve_btfids/string.o"
	)
	for _, test := range []struct {
		name      string
		exact     bool
		wantFlags string
	}{
		{name: "logical fallback", wantFlags: "-DLOGICAL"},
		{
			name:      "exact rooted name wins",
			exact:     true,
			wantFlags: "-DEXACT",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			variables := map[string]string{"obj": directory}
			if test.exact {
				variables["HOSTCFLAGS___LINUX_BZL_OBJECT_TREE__/resolve_btfids"] = "-DEXACT"
			}
			profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "tools/build/Makefile.build", directory, `
HOSTCFLAGS_resolve_btfids = -DLOGICAL
resolve_btfids-y = first.o second.o
host_c_flags = $(HOSTCFLAGS_$(obj))
selected = $(host_c_flags) -I$(obj)/generated $($(obj)-y)
`, variables)
			injected, err := compactKbuildSourceScriptInjectionsForTarget(
				profile, target, "string", nil, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			values, err := EvaluateCompactKbuildTarget(
				profile, target, "string", nil, nil, injected, "selected",
			)
			if err != nil {
				t.Fatal(err)
			}
			want := test.wantFlags +
				" -I__LINUX_BZL_OBJECT_TREE__/resolve_btfids/generated first.o second.o"
			if got := values["selected"]; got != want {
				t.Fatalf("selected = %q, want %q", got, want)
			}
		})
	}
}

func TestComputedVariableNameProjectionRetainsLogicalUndefinedName(t *testing.T) {
	parser := &kbuildParser{
		vars:      map[string]kbuildVariable{},
		undefined: map[string]bool{"HOSTCFLAGS_resolve_btfids": true},
		renderedValueProjections: []kbuildValueProjection{{
			rendered: "__LINUX_BZL_OBJECT_TREE__/resolve_btfids",
			logical:  "resolve_btfids",
		}},
	}
	const rendered = "HOSTCFLAGS___LINUX_BZL_OBJECT_TREE__/resolve_btfids"
	if got, want := parser.projectComputedVariableName(rendered), "HOSTCFLAGS_resolve_btfids"; got != want {
		t.Fatalf("projected undefined variable name = %q, want %q", got, want)
	}
}

func TestKbuildCompileCommandUsesExactTargetEvaluation(t *testing.T) {
	const (
		directory = "drivers/example"
		target    = directory + "/foo.o"
		source    = directory + "/foo.c"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
base-flags = -DMEASURED
cmd_cc_o_c = $(CC) $(base-flags) $(private-flag) -c -o $@ $<
$(obj)/%.o: private private-flag = -DTARGET_$*
$(obj)/%.o: $(src)/%.c
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"), "obj": directory, "src": directory,
	})
	metadata := &CompactMetadata{actionRoles: testConfiguredScopedActionRoles}
	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	sourceID, err := metadata.ensureActionPlanSource(plan, source)
	if err != nil {
		t.Fatal(err)
	}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "foo", lookupTarget: target, command: "cc_o_c",
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 || plan.Nodes[0].ID != producer {
		t.Fatalf("compile nodes = %#v, producer %q", plan.Nodes, producer)
	}
	node := plan.Nodes[0]
	recipe := plan.Recipes[node.Recipe]
	if node.Kind != "compile" || node.Tool != "cc" || node.Outputs[0] != (ActionPlanOutput{Tree: "objects", Path: target}) {
		t.Fatalf("compile node = %#v", node)
	}
	want := []string{"-DMEASURED", "-DTARGET_foo", "-c", "-o", "${output:00000000}", "${source:object:00000000}"}
	if !reflect.DeepEqual(recipe.Arguments, want) {
		t.Fatalf("compile arguments = %q, want %q", recipe.Arguments, want)
	}
}

func TestKbuildCompileCommandSplitsLinkMoveAndObjtool(t *testing.T) {
	const (
		directory = "drivers/example"
		target    = directory + "/foo.o"
		source    = directory + "/foo.c"
		objtool   = "tools/objtool/objtool"
	)
	profile := mustCompactKbuildProfileForTest(t, "build:"+directory, "scripts/Makefile.build", directory, `
cmd_objtool = ; $(objtool) --measured $@
cmd_ld_single = ; $(LD) -r -o $(obj)/.$(notdir $@).tmp $@; mv $(obj)/.$(notdir $@).tmp $@
cmd_cc_o_c = $(CC) -DCHAIN -c -o $@ $< $(cmd_ld_single) $(cmd_objtool)
$(obj)/%.o: $(src)/%.c
`, map[string]string{
		"CC": KbuildActionRoleToken("target", "cc"), "LD": KbuildActionRoleToken("target", "ld"), "objtool": "__LINUX_BZL_OBJECT_TREE__/" + objtool,
		"obj": directory, "src": directory,
	})
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
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
	builder := newCompactKbuildRulePlanBuilder(metadata, plan)
	producer, err := builder.buildCommandTemplate(target, compactKbuildRuleMatch{
		profile: profile, stem: "foo", command: "cc_o_c",
	}, []compactKbuildRuleInput{{path: source, sourceID: sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 4 {
		t.Fatalf("compile chain has %d nodes, want seed + compile + link + objtool: %#v", len(plan.Nodes), plan.Nodes)
	}
	final, ok := compactKbuildPlanNode(plan, producer)
	if !ok || final.Kind != "generate" || final.Tool != "generated" || final.Outputs[0] != (ActionPlanOutput{Tree: "objects", Path: target}) {
		t.Fatalf("final compile node = %#v", final)
	}
	if !slices.ContainsFunc(final.Inputs, func(input ActionPlanNodeEdge) bool { return input.ProducerID == objtoolProducer }) {
		t.Fatalf("objtool inputs = %#v, want generated tool %q", final.Inputs, objtoolProducer)
	}
	linked := ActionPlanNode{}
	for _, input := range final.Inputs {
		candidate, found := compactKbuildPlanNode(plan, input.ProducerID)
		if found && candidate.Kind == "link-relocatable" {
			linked = candidate
			break
		}
	}
	ok = linked.ID != ""
	if !ok || linked.Kind != "link-relocatable" || len(linked.Inputs) != 1 {
		t.Fatalf("link node = %#v", linked)
	}
	compiled, ok := compactKbuildPlanNode(plan, linked.Inputs[0].ProducerID)
	if !ok || compiled.Kind != "compile" {
		t.Fatalf("compile node = %#v", compiled)
	}
	linkRecipe := plan.Recipes[linked.Recipe]
	if got, want := linkRecipe.Arguments, []string{"-r", "-o", "drivers/example/.foo.o.tmp", "${input:prerequisite:00000000}"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("link arguments = %q, want %q", got, want)
	}
	if got, want := linkRecipe.WorkingOutputs["00000000"], "drivers/example/.foo.o.tmp"; got != want {
		t.Fatalf("link working output = %q, want %q", got, want)
	}
	objtoolRecipe := plan.Recipes[final.Recipe]
	if got, want := objtoolRecipe.Arguments, []string{"--measured", "../../" + target}; !reflect.DeepEqual(got, want) {
		t.Fatalf("objtool arguments = %q, want %q", got, want)
	}
	if got := objtoolRecipe.WorkingOutputs["00000000"]; got != target {
		t.Fatalf("objtool working output = %q, want %q", got, target)
	}
}
