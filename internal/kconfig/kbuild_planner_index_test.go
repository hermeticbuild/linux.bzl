package kconfig

import (
	"fmt"
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
