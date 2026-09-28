package kconfig

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompactKbuildPlannerIndexesExactDeclarations(t *testing.T) {
	const declarationCount = 4096
	profile := CompactKbuildProfile{
		Name:      "build:growth",
		Directory: "drivers/growth",
		evaluator: newKbuildTargetEvaluator(newKbuildParser(map[string]string{"VALUE": "selected"}, t.TempDir())),
	}
	for index := 0; index < declarationCount; index++ {
		target := fmt.Sprintf("drivers/growth/item-%04d.o", index)
		profile.Rules = append(profile.Rules, KbuildRule{Targets: []string{target}})
		profile.TargetVariables = append(profile.TargetVariables, KbuildTargetVariable{
			Targets: []string{target}, Variable: "FLAGS", Value: fmt.Sprintf("flag-%04d", index),
		})
	}
	profile.Rules = append(profile.Rules, KbuildRule{Targets: []string{"drivers/growth/%.o"}})
	profile.TargetVariables = append(profile.TargetVariables, KbuildTargetVariable{
		Targets: []string{"drivers/growth/%.o"}, Variable: "PATTERN", Value: "pattern",
	})

	runtime := compactKbuildPlannerRuntimeForProfile(profile)
	if got := len(runtime.exactIndex); got != declarationCount {
		t.Fatalf("exact rule index has %d keys, want %d", got, declarationCount)
	}
	if got := len(runtime.exact); got != declarationCount {
		t.Fatalf("exact declaration storage has %d entries, want %d", got, declarationCount)
	}
	if got := len(runtime.patternIndex); got != 1 {
		t.Fatalf("pattern prefix index has %d entries, want 1", got)
	}
	if got := len(runtime.patterns); got != 1 || len(runtime.patterns[0].rules) != 1 || len(runtime.patterns[0].variables) != 1 {
		t.Fatalf("shared pattern declaration storage = %#v, want one rule and one variable", runtime.patterns)
	}

	for index := 0; index < declarationCount; index++ {
		target := fmt.Sprintf("drivers/growth/item-%04d.o", index)
		if candidates := compactKbuildRuleCandidates(profile, target); len(candidates) != 2 {
			t.Fatalf("target %q has %d rule candidates, want exact plus pattern", target, len(candidates))
		}
		if variables := compactKbuildTargetVariables(profile, target); len(variables) != 2 {
			t.Fatalf("target %q has %d target variables, want exact plus pattern", target, len(variables))
		}
	}
	if again := compactKbuildPlannerRuntimeForProfile(profile); again != runtime {
		t.Fatal("profile planner index was rebuilt")
	}
}

func TestCompactKbuildPlannerReindexesSharedEvaluatorForInvocationDirectory(t *testing.T) {
	evaluator := newKbuildTargetEvaluator(newKbuildParser(nil, t.TempDir()))
	profile := CompactKbuildProfile{
		Name: "shared", Directory: "first", evaluator: evaluator,
		Rules: []KbuildRule{{Targets: []string{"result.o"}}},
	}
	first := compactKbuildPlannerRuntimeForProfile(profile)
	if got := len(compactKbuildRuleCandidates(profile, "first/result.o")); got != 1 {
		t.Fatalf("first invocation candidates = %d, want 1", got)
	}
	profile.Directory = "second"
	second := compactKbuildPlannerRuntimeForProfile(profile)
	if second == first {
		t.Fatal("distinct invocation directory reused a stale planner index")
	}
	if got := len(compactKbuildRuleCandidates(profile, "second/result.o")); got != 1 {
		t.Fatalf("second invocation candidates = %d, want 1", got)
	}
}

func TestCompactKbuildPlannerPatternIndexVisitsCompatiblePrefixes(t *testing.T) {
	profile := CompactKbuildProfile{
		Name:      "build:selected",
		Directory: "drivers",
		evaluator: newKbuildTargetEvaluator(newKbuildParser(nil, t.TempDir())),
	}
	for index := 0; index < 4096; index++ {
		pattern := fmt.Sprintf("drivers/family-%04d/%%.o", index)
		profile.Rules = append(profile.Rules, KbuildRule{Targets: []string{pattern}})
		profile.TargetVariables = append(profile.TargetVariables, KbuildTargetVariable{
			Targets: []string{pattern}, Variable: "FLAGS", Value: pattern,
		})
	}
	profile.Rules = append(profile.Rules, KbuildRule{Targets: []string{"drivers/family-2048/%.o"}})
	profile.TargetVariables = append(profile.TargetVariables, KbuildTargetVariable{
		Targets: []string{"drivers/family-2048/%.o"}, Variable: "SELECTED", Value: "yes",
	})

	target := "drivers/family-2048/selected.o"
	runtime := compactKbuildPlannerRuntimeForProfile(profile)
	if got := len(runtime.patternIndex); got != 4096 {
		t.Fatalf("pattern prefix index has %d keys, want 4096", got)
	}
	if got := len(runtime.matchingPatternRules(target)); got != 2 {
		t.Fatalf("compatible rule candidates = %d, want 2", got)
	}
	if got := len(runtime.matchingPatternVariables(target)); got != 2 {
		t.Fatalf("compatible variable candidates = %d, want 2", got)
	}
	if got := len(compactKbuildRuleCandidates(profile, target)); got != 2 {
		t.Fatalf("resolved rule candidates = %d, want 2", got)
	}
	if got := len(compactKbuildTargetVariables(profile, target)); got != 2 {
		t.Fatalf("resolved target variables = %d, want 2", got)
	}
}

// resolveCompactKbuildProfileSourcePathByScanForTest is the former linear
// resolver retained as an equivalence oracle for unambiguous source-root
// configurations.
func resolveCompactKbuildProfileSourcePathByScanForTest(profile CompactKbuildProfile, sourcePath string) (string, bool) {
	if profile.evaluator == nil || profile.evaluator.template == nil {
		return "", false
	}
	sourcePath = canonicalKbuildRulePath(sourcePath)
	if err := validatePlanRelativePath("Kbuild source path", sourcePath); err != nil {
		return "", false
	}
	type sourceRoot struct {
		graphPrefix string
		physical    string
	}
	selected := sourceRoot{}
	selectedSet := false
	for rawPrefix, root := range profile.evaluator.template.sourceRoots {
		prefix := filepath.ToSlash(rawPrefix)
		graphPrefix := ""
		switch {
		case prefix == "__LINUX_BZL_SOURCE_TREE__":
		case strings.HasPrefix(prefix, "__LINUX_BZL_SOURCE_TREE__/"):
			graphPrefix = strings.TrimPrefix(prefix, "__LINUX_BZL_SOURCE_TREE__/")
			if validatePlanRelativePath("Kbuild source-tree overlay", graphPrefix) != nil {
				continue
			}
		case strings.HasPrefix(prefix, "__LINUX_BZL_"):
			continue
		default:
			graphPrefix = prefix
			if validatePlanRelativePath("Kbuild configured source root", graphPrefix) != nil {
				continue
			}
		}
		if graphPrefix != "" && sourcePath != graphPrefix && !strings.HasPrefix(sourcePath, graphPrefix+"/") {
			continue
		}
		if !selectedSet || len(graphPrefix) > len(selected.graphPrefix) {
			selected = sourceRoot{graphPrefix: graphPrefix, physical: root}
			selectedSet = true
		} else if len(graphPrefix) == len(selected.graphPrefix) && graphPrefix == selected.graphPrefix &&
			filepath.Clean(root) != filepath.Clean(selected.physical) {
			return "", false
		}
	}
	if !selectedSet || strings.TrimSpace(selected.physical) == "" {
		return "", false
	}
	relative := strings.TrimPrefix(sourcePath, selected.graphPrefix)
	relative = strings.TrimPrefix(relative, "/")
	return filepath.Join(selected.physical, filepath.FromSlash(relative)), true
}

func TestCompactKbuildPlannerSourceRootIndexMatchesLinearResolution(t *testing.T) {
	parser := newKbuildParser(nil, t.TempDir())
	parser.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":                  "/source/linux",
		"__LINUX_BZL_SOURCE_TREE__/drivers/vendor":   "/source/vendor",
		"external/rust/library":                      "/source/rust/library",
		"__LINUX_BZL_OBJECT_TREE__":                  "/object/linux",
		"__LINUX_BZL_HOST_DEPS__":                    "/host/deps",
		"__LINUX_BZL_SOURCE_TREE__/../invalid":       "/invalid/overlay",
		"external/invalid/../configured-source-root": "/invalid/configured",
	}
	profile := CompactKbuildProfile{Name: "source-roots", evaluator: newKbuildTargetEvaluator(parser)}
	for _, sourcePath := range []string{
		"drivers/vendor/device.c",
		"drivers/vendor",
		"drivers/vendor-shadow/device.c",
		"drivers/other/device.c",
		"external/rust/library/core/src/lib.rs",
		"external/rust/library",
		"external/rust/library-shadow/core/src/lib.rs",
		"include/generated/autoconf.h",
		"__LINUX_BZL_HOST_DEPS__/include/libelf.h",
		"../escape.c",
		"/absolute.c",
	} {
		got, ok := ResolveCompactKbuildProfileSourcePath(profile, sourcePath)
		want, wantOK := resolveCompactKbuildProfileSourcePathByScanForTest(profile, sourcePath)
		if ok != wantOK || filepath.Clean(got) != filepath.Clean(want) {
			t.Errorf("indexed resolution of %q = (%q, %t), linear resolution = (%q, %t)", sourcePath, got, ok, want, wantOK)
		}
	}
	runtime := compactKbuildPlannerRuntimeForProfile(profile)
	if got, want := len(runtime.sourcePathRoots), 3; got != want {
		t.Fatalf("compiled source-root prefix count = %d, want %d valid source roots", got, want)
	}
}

func TestCompactKbuildPlannerSourceRootIndexFailsClosedOnAmbiguousPrefix(t *testing.T) {
	parser := newKbuildParser(nil, t.TempDir())
	parser.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":                        "/source/linux",
		"__LINUX_BZL_SOURCE_TREE__/drivers/vendor":         "/source/vendor-a",
		"drivers/vendor":                                   "/source/vendor-b",
		"__LINUX_BZL_SOURCE_TREE__/drivers/vendor/special": "/source/special",
	}
	profile := CompactKbuildProfile{Name: "ambiguous-source-roots", evaluator: newKbuildTargetEvaluator(parser)}
	for _, sourcePath := range []string{"drivers/vendor", "drivers/vendor/device.c"} {
		if got, ok := ResolveCompactKbuildProfileSourcePath(profile, sourcePath); ok || got != "" {
			t.Errorf("ambiguous resolution of %q = (%q, %t), want fail-closed", sourcePath, got, ok)
		}
	}
	if got, ok := ResolveCompactKbuildProfileSourcePath(profile, "drivers/vendor/special/device.c"); !ok || filepath.Clean(got) != "/source/special/device.c" {
		t.Fatalf("more-specific unambiguous overlay resolution = (%q, %t), want /source/special/device.c", got, ok)
	}
	if got, ok := ResolveCompactKbuildProfileSourcePath(profile, "drivers/other/device.c"); !ok || filepath.Clean(got) != "/source/linux/drivers/other/device.c" {
		t.Fatalf("unambiguous kernel-root resolution = (%q, %t), want /source/linux/drivers/other/device.c", got, ok)
	}

	parser = newKbuildParser(nil, t.TempDir())
	parser.sourceRoots = map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":                "/source/linux",
		"__LINUX_BZL_SOURCE_TREE__/drivers/vendor": "/source/vendor/.",
		"drivers/vendor":                           "/source/vendor",
	}
	profile = CompactKbuildProfile{Name: "equivalent-source-root-aliases", evaluator: newKbuildTargetEvaluator(parser)}
	if got, ok := ResolveCompactKbuildProfileSourcePath(profile, "drivers/vendor/device.c"); !ok || filepath.Clean(got) != "/source/vendor/device.c" {
		t.Fatalf("equivalent physical aliases resolution = (%q, %t), want /source/vendor/device.c", got, ok)
	}
}

func TestResolveCompactKbuildMakeTargetUsesGraphThenInvocationIdentity(t *testing.T) {
	profile := CompactKbuildProfile{Name: "split-root", Directory: "libsubcmd"}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: "tools/lib/subcmd",
	}); err != nil {
		t.Fatal(err)
	}
	const rootedTarget = "tools/objtool/libsubcmd/exec-cmd.o"
	for _, test := range []struct {
		name, target, makeTarget, want, plannerWant string
		valid                                       bool
	}{
		{
			name: "empty is invalid", target: rootedTarget,
			want: rootedTarget, valid: false, plannerWant: "",
		},
		{
			name: "workspace rooted output", target: rootedTarget, makeTarget: rootedTarget,
			want: rootedTarget, valid: true,
		},
		{
			name: "object marker", target: rootedTarget,
			makeTarget: "__LINUX_BZL_OBJECT_TREE__/" + rootedTarget,
			want:       rootedTarget, valid: true,
		},
		{
			name: "source marker", target: rootedTarget,
			makeTarget: "__LINUX_BZL_SOURCE_TREE__/" + rootedTarget,
			want:       rootedTarget, valid: true,
		},
		{
			name: "private object marker", target: rootedTarget,
			makeTarget: compactKbuildActionObjectTreeMarker + "/" + rootedTarget,
			want:       rootedTarget, valid: true,
		},
		{
			name: "invocation relative", target: "tools/lib/subcmd/exec-cmd.o",
			makeTarget: "exec-cmd.o", want: "tools/lib/subcmd/exec-cmd.o", valid: true,
		},
		{
			name: "logical object relative", target: "libsubcmd/exec-cmd.o",
			makeTarget: "exec-cmd.o", want: "libsubcmd/exec-cmd.o", valid: true,
		},
		{
			name: "parent traversal", target: "virt/kvm/kvm_main.o",
			makeTarget: "arch/x86/kvm/../../../virt/kvm/kvm_main.o",
			want:       "arch/x86/kvm/../../../virt/kvm/kvm_main.o", valid: true,
		},
		{
			name: "unrelated alias", target: rootedTarget, makeTarget: "unrelated/exec-cmd.o",
			want: rootedTarget, valid: false, plannerWant: "",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, valid := ResolveCompactKbuildMakeTarget(profile, test.target, test.makeTarget)
			if got != test.want || valid != test.valid {
				t.Fatalf("resolved Make target = (%q, %t), want (%q, %t)", got, valid, test.want, test.valid)
			}
			plannerWant := test.plannerWant
			if test.valid {
				plannerWant = test.want
			}
			if planner := compactKbuildRuleLookupTarget(profile, test.target, test.makeTarget); planner != plannerWant {
				t.Fatalf("planner Make target = %q, want %q", planner, plannerWant)
			}
		})
	}
}

func TestNormalizeCompactKbuildSelectionMakeTargetsUsesSharedIdentity(t *testing.T) {
	profile := CompactKbuildProfile{Name: "split-root", Path: "tools/build/Makefile.build", Directory: "libsubcmd"}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree, Directory: "tools/lib/subcmd",
	}); err != nil {
		t.Fatal(err)
	}
	const (
		rootedTarget = "tools/objtool/libsubcmd/exec-cmd.o"
		parentTarget = "virt/kvm/kvm_main.o"
		parentAlias  = "arch/x86/kvm/../../../virt/kvm/kvm_main.o"
	)
	selections := []CompactKbuildSelection{
		{Profile: profile.Name, Target: rootedTarget, MakeTarget: rootedTarget},
		{Profile: profile.Name, Target: parentTarget, MakeTarget: parentAlias},
	}
	normalized, err := normalizeCompactKbuildSelectionMakeTargets([]CompactKbuildProfile{profile}, selections)
	if err != nil {
		t.Fatal(err)
	}
	if got := normalized[0].MakeTarget; got != rootedTarget {
		t.Fatalf("rooted Make target = %q, want %q", got, rootedTarget)
	}
	if got := normalized[1].MakeTarget; got != parentAlias {
		t.Fatalf("parent-traversal Make target = %q, want %q", got, parentAlias)
	}
	direct, err := normalizeCompactKbuildSelectionMakeTarget(profile, selections[1])
	if err != nil {
		t.Fatal(err)
	}
	if direct != normalized[1] {
		t.Fatalf("direct normalized selection = %#v, want bulk result %#v", direct, normalized[1])
	}

	_, err = normalizeCompactKbuildSelectionMakeTargets(
		[]CompactKbuildProfile{profile},
		[]CompactKbuildSelection{{Profile: profile.Name, Target: rootedTarget}},
	)
	if err == nil || !strings.Contains(err.Error(), "has empty lexical Make target") {
		t.Fatalf("empty lexical Make target error = %v", err)
	}

	_, err = normalizeCompactKbuildSelectionMakeTargets(
		[]CompactKbuildProfile{profile},
		[]CompactKbuildSelection{{
			Profile: profile.Name, Target: rootedTarget, MakeTarget: "unrelated/exec-cmd.o",
		}},
	)
	if err == nil || !strings.Contains(err.Error(),
		`lexical Make target "unrelated/exec-cmd.o" resolves to root target "unrelated/exec-cmd.o" and invocation target "tools/lib/subcmd/unrelated/exec-cmd.o", want canonical target "tools/objtool/libsubcmd/exec-cmd.o"`,
	) {
		t.Fatalf("unrelated lexical alias error = %v", err)
	}
}

func BenchmarkCompactKbuildPlannerPatternLookup(b *testing.B) {
	const declarationCount = 16384
	profile := CompactKbuildProfile{
		Name:      "build:benchmark",
		Directory: "drivers",
		evaluator: newKbuildTargetEvaluator(newKbuildParser(nil, b.TempDir())),
	}
	for index := 0; index < declarationCount; index++ {
		pattern := fmt.Sprintf("drivers/family-%05d/%%.o", index)
		profile.Rules = append(profile.Rules, KbuildRule{Targets: []string{pattern}})
		profile.TargetVariables = append(profile.TargetVariables, KbuildTargetVariable{
			Targets: []string{pattern}, Variable: "FLAGS", Value: pattern,
		})
	}
	target := "drivers/family-08192/selected.o"
	compactKbuildPlannerRuntimeForProfile(profile)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := len(compactKbuildRuleCandidates(profile, target)); got != 1 {
			b.Fatalf("rule candidates = %d, want 1", got)
		}
		if got := len(compactKbuildTargetVariables(profile, target)); got != 1 {
			b.Fatalf("target variables = %d, want 1", got)
		}
	}
}

func BenchmarkResolveCompactKbuildProfileSourcePathScale(b *testing.B) {
	const sourceRootCount = 16384
	parser := newKbuildParser(nil, b.TempDir())
	parser.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": "/source/linux"}
	for index := 0; index < sourceRootCount; index++ {
		prefix := fmt.Sprintf("external/source-%05d", index)
		parser.sourceRoots[prefix] = filepath.Join("/source/configured", fmt.Sprintf("%05d", index))
	}
	profile := CompactKbuildProfile{Name: "source-root-scale", evaluator: newKbuildTargetEvaluator(parser)}
	const sourcePath = "external/source-08192/include/generated.h"
	want := filepath.Clean("/source/configured/08192/include/generated.h")
	if got, ok := ResolveCompactKbuildProfileSourcePath(profile, sourcePath); !ok || filepath.Clean(got) != want {
		b.Fatalf("indexed resolution = (%q, %t), want %q", got, ok, want)
	}

	b.Run("indexed", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if got, ok := ResolveCompactKbuildProfileSourcePath(profile, sourcePath); !ok || filepath.Clean(got) != want {
				b.Fatalf("indexed resolution = (%q, %t), want %q", got, ok, want)
			}
		}
	})
	b.Run("linear-reference", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if got, ok := resolveCompactKbuildProfileSourcePathByScanForTest(profile, sourcePath); !ok || filepath.Clean(got) != want {
				b.Fatalf("linear resolution = (%q, %t), want %q", got, ok, want)
			}
		}
	})
}

func TestEvaluateCompactKbuildTargetKeepsResultsCallerOwned(t *testing.T) {
	parser := newKbuildParser(map[string]string{"VALUE": "selected"}, t.TempDir())
	profile := CompactKbuildProfile{Name: "cached", evaluator: newKbuildTargetEvaluator(parser)}

	first, err := EvaluateCompactKbuildTarget(profile, "target.o", "target", []string{"target.c"}, nil, nil, "VALUE")
	if err != nil {
		t.Fatal(err)
	}
	first["VALUE"] = "mutated by caller"
	second, err := EvaluateCompactKbuildTarget(profile, "target.o", "target", []string{"target.c"}, nil, nil, "VALUE")
	if err != nil {
		t.Fatal(err)
	}
	if got := second["VALUE"]; got != "selected" {
		t.Fatalf("cached VALUE = %q, want selected", got)
	}
}

func TestCompactKbuildPlannerExactVariablesUseScopedMakeIdentity(t *testing.T) {
	const directory = ".linux-bzl/external/scoped"
	profile := mustCompactKbuildProfileForTest(t, "exact-variable-scope", "scripts/Makefile.build", directory, `
%.o: PATTERN := pattern
result.o ./result.o ././result.o: EXACT := exact
$(objtree)/elsewhere/rooted.o: ROOTED := rooted
__LINUX_BZL_OBJECT_TREE__/elsewhere/rooted.o: LITERAL := must-not-acquire-root
`, map[string]string{"objtree": "__LINUX_BZL_OBJECT_TREE__"})
	// Only the injected root carries planner authority; the same bytes authored
	// literally in source must stay protected rather than alias the rooted target.
	if got, want := profile.TargetVariables[2].Targets[0], "__LINUX_BZL_OBJECT_TREE__/elsewhere/rooted.o"; got != want {
		t.Fatalf("captured rooted declaration = %q, want %q", got, want)
	}
	if !compactKbuildContainsProtectedLiteralActionMarker(profile.TargetVariables[3].Targets[0]) {
		t.Fatal("source-literal root marker acquired planner authority")
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: directory,
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ target, word, want string }{
		{directory + "/result.o", "result.o", "PATTERN,EXACT"},
		{directory + "/result.o", "./result.o", "PATTERN,EXACT"},
		{directory + "/result.o", directory + "/result.o", "PATTERN,EXACT"},
		{directory + "/result.o", "__LINUX_BZL_OBJECT_TREE__/" + directory + "/result.o", "PATTERN,EXACT"},
		{directory + "/other.o", "other.o", "PATTERN"},
		{"elsewhere/rooted.o", "__LINUX_BZL_OBJECT_TREE__/elsewhere/rooted.o", "PATTERN,ROOTED"},
		{directory + "/result.o", "unrelated/result.o", ""},
	} {
		variables := compactKbuildTargetVariablesForMakeTarget(profile, tc.target, tc.word)
		var names []string
		for _, variable := range variables {
			names = append(names, variable.Variable)
		}
		if got := strings.Join(names, ","); got != tc.want {
			t.Errorf("target=%q word=%q variables=%q want=%q", tc.target, tc.word, got, tc.want)
		}
	}
	firstRuntime := compactKbuildPlannerRuntimeForProfile(profile)
	sibling := profile
	sibling.Directory = ".linux-bzl/external/sibling"
	if err := SetCompactKbuildProfileInvocationLocation(&sibling, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree, Directory: sibling.Directory,
	}); err != nil {
		t.Fatal(err)
	}
	if compactKbuildPlannerRuntimeForProfile(sibling) == firstRuntime {
		t.Fatal("sibling invocation reused the first exact variable index")
	}
	variables := compactKbuildTargetVariablesForMakeTarget(sibling, sibling.Directory+"/result.o", "result.o")
	if len(variables) != 2 || variables[1].Variable != "EXACT" {
		t.Fatalf("sibling lost invocation-local exact assignment: %#v", variables)
	}
	if firstRuntime.exactDeclarations(sibling.Directory+"/result.o") != nil {
		t.Fatal("indexing a sibling mutated the first invocation index")
	}
}

func TestCompactKbuildPlannerScopedExactVariablesDoNotMergeLexicalAliases(t *testing.T) {
	const target = "virt/kvm/kvm_main.o"
	const lexical = "arch/x86/kvm/../../../virt/kvm/kvm_main.o"
	profile := mustCompactKbuildProfileForTest(t, "exact-variable-alias", "scripts/Makefile.build", "arch/x86/kvm", `
virt/kvm/kvm_main.o: CANONICAL := must-not-leak
arch/x86/kvm/../../../virt/kvm/kvm_main.o: LEXICAL := lexical
arch/x86/kvm/%.o: PATTERN := lexical-pattern
`, nil)
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ word, want string }{
		{lexical, "LEXICAL,PATTERN"},
		{"./" + lexical, "LEXICAL,PATTERN"},
		{"__LINUX_BZL_OBJECT_TREE__/" + lexical, "LEXICAL,PATTERN"},
		{target, "CANONICAL"},
		{"unrelated/kvm_main.o", ""},
	} {
		variables := compactKbuildTargetVariablesForMakeTarget(profile, target, tc.word)
		var names []string
		for _, variable := range variables {
			names = append(names, variable.Variable)
		}
		if got := strings.Join(names, ","); got != tc.want {
			t.Errorf("Make word %q variables=%q want=%q", tc.word, got, tc.want)
		}
	}
}
