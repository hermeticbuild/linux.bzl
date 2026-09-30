package kconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type selectedControlTestFiles struct {
	files   map[string]testKbuildVirtualFile
	matches map[string][]string
	err     map[string]error
}

func (f selectedControlTestFiles) Match(pattern string) []string {
	return append([]string(nil), f.matches[pattern]...)
}

func (f selectedControlTestFiles) Read(path string) (string, bool, bool, error) {
	if err := f.err[path]; err != nil {
		return "", false, false, err
	}
	file, exists := f.files[path]
	return file.content, exists, file.exact, nil
}

func selectedControlTestProfile(t *testing.T, source string, extraVariables ...map[string]string) (CompactKbuildProfile, string, string) {
	t.Helper()
	dir := t.TempDir()
	sourceRoot := filepath.Join(dir, "source")
	objectRoot := filepath.Join(dir, "object")
	for _, root := range []string{sourceRoot, objectRoot} {
		if err := os.MkdirAll(filepath.Join(root, "include", "config"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	makefile := filepath.Join(sourceRoot, "Makefile")
	if err := os.WriteFile(makefile, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	variables := map[string]string{
		"objtree": "__LINUX_BZL_OBJECT_TREE__",
		"srctree": "__LINUX_BZL_SOURCE_TREE__",
	}
	if len(extraVariables) > 1 {
		t.Fatal("selected control fixture accepts one optional source variable set")
	}
	if len(extraVariables) != 0 {
		for name, value := range extraVariables[0] {
			variables[name] = value
		}
	}
	parsed, err := ParseKbuildFileTree(makefile, KbuildOptions{
		RootDir: sourceRoot, WorkingDir: objectRoot,
		Variables: variables,
		SourceRoots: map[string]string{
			"__LINUX_BZL_SOURCE_TREE__": sourceRoot,
			"__LINUX_BZL_OBJECT_TREE__": objectRoot,
		},
		EnvironmentVariables:  map[string]string{"INHERITED": "incoming"},
		MakeVariablesComplete: true, CaptureTargetEvaluator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewCompactKbuildProfile("root", makefile, sourceRoot, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationObjectTree,
	}); err != nil {
		t.Fatal(err)
	}
	profile.EntryTargets = []string{"all"}
	return profile, sourceRoot, objectRoot
}

func selectedControlTestRuleIndex(t *testing.T, profile CompactKbuildProfile, target string) int {
	t.Helper()
	for i, rule := range profile.Rules {
		for _, declared := range rule.Targets {
			if declared == target {
				return i
			}
		}
	}
	t.Fatalf("profile %q has no selected target %q", profile.Name, target)
	return -1
}

func selectedControlTestFrontier(id string, files selectedControlTestFiles, artifact KbuildControlReadArtifact) KbuildControlRecipeFrontier {
	return KbuildControlRecipeFrontier{
		ID: id, Files: files,
		ResolveArtifact: func(path string) (KbuildControlReadArtifact, bool, error) {
			if _, exists := files.files[path]; !exists {
				return KbuildControlReadArtifact{}, false, nil
			}
			return artifact, true, nil
		},
	}
}

func selectedControlTestRecipeSnapshot(evaluation KbuildControlEvaluation, target string, ruleIndex, recipeIndex int) (*KbuildSelectedControlRecipeSnapshot, bool) {
	for _, snapshot := range CompactKbuildSelectedControlRecipeSnapshots(evaluation.Profile, target) {
		if snapshot.Line.RuleIndex == ruleIndex && snapshot.Line.RecipeIndex == recipeIndex {
			return snapshot, true
		}
	}
	return nil, false
}

func TestSelectedKbuildSecondExpansionUsesStateBeforeRecipeEval(t *testing.T) {
	for _, withExecutable := range []bool{false, true} {
		t.Run(fmt.Sprintf("executable=%t", withExecutable), func(t *testing.T) {
			source := `
prerequisite := before.c
.SECONDEXPANSION:
all: target.o
%.o: $$(prerequisite)
	$(eval prerequisite := after.c)
`
			if withExecutable {
				source += "\t@echo $(prerequisite) > $@\n"
			}
			profile, _, _ := selectedControlTestProfile(t, source)
			stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := stepper.BeginTarget("target.o", "target.o", "all"); err != nil {
				t.Fatal(err)
			}
			rule := selectedControlTestRuleIndex(t, profile, "%.o")
			frontier := selectedControlTestFrontier("before-target", selectedControlTestFiles{}, KbuildControlReadArtifact{})
			count := 1
			if withExecutable {
				count++
			}
			for recipeIndex := range count {
				line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
					Target: "target.o", RuleIndex: rule, RecipeIndex: recipeIndex,
					AutomaticTarget: "target.o", Stem: "target", Normal: []string{"before.c"},
				}, frontier)
				if err != nil {
					t.Fatal(err)
				}
				if recipeIndex == 1 {
					values, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "target.o", "target", nil, nil, nil, "prerequisite")
					if err != nil || values["prerequisite"] != "after.c" {
						t.Fatalf("later executable recipe Make value = %#v, %v, want after.c", values, err)
					}
				}
				if err := stepper.ApplyRecipe(line); err != nil {
					t.Fatal(err)
				}
			}
			evaluation, err := stepper.Finish(frontier)
			if err != nil {
				t.Fatal(err)
			}
			entry := CompactKbuildSelectedControlRuleEntrySnapshot(evaluation.Profile, "target.o")
			if entry == nil || entry.Line.RecipeIndex != 0 {
				t.Fatalf("second expansion entry = %#v, want first source recipe before eval", entry)
			}
			context, err := EvaluateCompactKbuildCandidatePrerequisitesForMakeTarget(
				evaluation.Profile, "target.o", "target.o", rule, "target",
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(context.Normal) != 1 || context.Normal[0].Target != "before.c" ||
				context.Normal[0].MakeTarget != "before.c" {
				t.Fatalf("second expansion prerequisites = %#v, want entry-state before.c", context.Normal)
			}
			match := compactKbuildRuleMatch{
				profile: evaluation.Profile, rule: evaluation.Profile.Rules[rule], ruleOrder: rule,
				lookupTarget: "target.o", stem: "target", resolved: true,
			}
			logical, err := compactKbuildRuleAutomaticEvaluationContext("target.o", match, nil)
			if err != nil || len(logical.normal) != 1 || logical.normal[0] != "before.c" {
				t.Fatalf("source-ordered automatic prerequisite = %#v, %v; want entry-state before.c", logical, err)
			}
			rooted, err := compactKbuildRuleRootedAutomaticEvaluationContext(
				"target.o", match, nil, map[string]string{"obj": "__LINUX_BZL_OBJECT_TREE__"},
			)
			if err != nil || len(rooted.normal) != 1 || !strings.HasSuffix(rooted.normal[0], "/before.c") {
				t.Fatalf("rooted automatic prerequisite = %#v, %v; want entry-state before.c", rooted, err)
			}
		})
	}
}

func TestSelectedKbuildControlStepperReadsAbsentBeforeWriterAndExactAfterWriter(t *testing.T) {
	profile, _, objectRoot := selectedControlTestProfile(t, `
KERNELRELEASE = $(file < include/config/kernel.release)
all:
	@echo $(KERNELRELEASE)
	@printf release > include/config/kernel.release
	@echo $(KERNELRELEASE)
`)
	physical := filepath.Join(objectRoot, "include", "config", "kernel.release")
	if err := os.WriteFile(physical, []byte("stale-analysis-host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(physical); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("stale physical object-root fixture Lstat = %v, %v", info, err)
	}
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	index := selectedControlTestRuleIndex(t, profile, "all")
	path := "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release"
	produced := KbuildControlReadArtifact{
		Tree: CompactKbuildInvocationObjectTree, Identity: "root/all/include/config/kernel.release",
		Version: "release-sha256:writer",
		Producer: CompactKbuildVisibleArtifact{
			Path: "include/config/kernel.release", Profile: "root", Target: "all",
		},
	}
	frontiers := []KbuildControlRecipeFrontier{
		selectedControlTestFrontier("pre-writer", selectedControlTestFiles{}, produced),
		selectedControlTestFrontier("writer-frontier", selectedControlTestFiles{}, produced),
		selectedControlTestFrontier("after-writer", selectedControlTestFiles{
			files: map[string]testKbuildVirtualFile{path: {content: "6.18.39-test\n", exact: true}},
		}, produced),
	}
	var before, after *KbuildSelectedControlRecipeSnapshot
	for recipeIndex, frontier := range frontiers {
		line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
			Target: "all", RuleIndex: index, RecipeIndex: recipeIndex,
		}, frontier)
		if err != nil {
			t.Fatalf("before recipe %d: %v", recipeIndex, err)
		}
		if recipeIndex == 0 || recipeIndex == 2 {
			values, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "all", "", nil, nil, nil, "KERNELRELEASE")
			if err != nil {
				t.Fatalf("recipe %d read: %v", recipeIndex, err)
			}
			want := ""
			if recipeIndex == 2 {
				want = "6.18.39-test"
			}
			if values["KERNELRELEASE"] != want {
				t.Fatalf("recipe %d KERNELRELEASE = %q, want %q", recipeIndex, values["KERNELRELEASE"], want)
			}
		}
		if err := stepper.ApplyRecipe(line); err != nil {
			t.Fatalf("apply recipe %d: %v", recipeIndex, err)
		}
		if recipeIndex == 0 {
			before = line
		}
		if recipeIndex == 2 {
			after = line
		}
	}
	if got := before.Reads(); len(got) != 1 || got[0].Path != path || got[0].Exists || got[0].FrontierID != "pre-writer" {
		t.Fatalf("pre-writer read = %#v, want exact absent observation", got)
	}
	if got := after.Reads(); len(got) != 1 || got[0].Path != path || !got[0].Exists || got[0].Artifact != produced || got[0].FrontierID != "" {
		t.Fatalf("post-writer read = %#v, want exact writer/version", got)
	}
	if before.ReadIdentity() == after.ReadIdentity() || before.ReadIdentity() == "" || after.ReadIdentity() == "" {
		t.Fatalf("absent/exact read identities aliased: %q, %q", before.ReadIdentity(), after.ReadIdentity())
	}
	evaluation, err := stepper.Finish(frontiers[2])
	if err != nil {
		t.Fatal(err)
	}
	for recipeIndex, expected := range map[int]*KbuildSelectedControlRecipeSnapshot{0: before, 2: after} {
		actual, ok := selectedControlTestRecipeSnapshot(evaluation, "all", index, recipeIndex)
		if !ok || actual != expected {
			t.Fatalf("persisted recipe %d snapshot = %p, %v, want %p", recipeIndex, actual, ok, expected)
		}

	}
	if _, err := EvaluateCompactKbuildTarget(evaluation.Profile, "all", "", nil, nil, nil, "KERNELRELEASE"); err == nil || !strings.Contains(err.Error(), "different file reads") {
		t.Fatalf("ambiguous target-wide evaluator error = %v, want per-line requirement", err)
	}
}

func TestSelectedKbuildControlStepperPreWriterAbsenceIgnoresPhysicalObjectFile(t *testing.T) {
	profile, _, objectRoot := selectedControlTestProfile(t, `
KERNELRELEASE = $(file < include/config/kernel.release)
all:
	@echo $(KERNELRELEASE)
`)
	physical := filepath.Join(objectRoot, "include", "config", "kernel.release")
	if err := os.WriteFile(physical, []byte("future-writer-stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
		Target: "all", RuleIndex: selectedControlTestRuleIndex(t, profile, "all"),
	}, selectedControlTestFrontier("before-release-writer", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	values, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "all", "", nil, nil, nil, "KERNELRELEASE")
	if err != nil || values["KERNELRELEASE"] != "" {
		t.Fatalf("pre-writer Make read = %#v, %v, want absent despite physical object file", values, err)
	}
	reads := line.Reads()
	if len(reads) != 1 || reads[0].Path != "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release" ||
		reads[0].Exists || reads[0].FrontierID != "before-release-writer" {
		t.Fatalf("pre-writer absent read provenance = %#v", reads)
	}
}

func TestSelectedKbuildControlStepperExactReadIdentitySharesUnchangedProducer(t *testing.T) {
	path := "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release"
	artifact := KbuildControlReadArtifact{
		Tree: CompactKbuildInvocationObjectTree, Identity: "root/all/release writer", Version: "release-v1",
		Producer: CompactKbuildVisibleArtifact{
			Path: "include/config/kernel.release", Profile: "root", Target: "all",
		},
	}
	files := selectedControlTestFiles{files: map[string]testKbuildVirtualFile{
		path: {content: "6.18.39-test\n", exact: true},
	}}
	var snapshots []*KbuildSelectedControlRecipeSnapshot
	for _, id := range []string{"after-writer", "after-unrelated-action"} {
		view := &kbuildControlRecipeReadView{frontier: selectedControlTestFrontier(id, files, artifact)}
		if _, exists, exact, err := view.Read(path); err != nil || !exists || !exact {
			t.Fatalf("frontier %q exact read = %v, %v, %v", id, exists, exact, err)
		}
		snapshots = append(snapshots, &KbuildSelectedControlRecipeSnapshot{FrontierID: id, view: view})
	}
	if snapshots[0].ReadIdentity() == "" || snapshots[0].ReadIdentity() != snapshots[1].ReadIdentity() {
		t.Fatalf("unchanged producer/version identities differ across unrelated frontiers: %q, %q",
			snapshots[0].ReadIdentity(), snapshots[1].ReadIdentity())
	}
}

func TestSelectedKbuildControlStepperRejectsConflictingFrozenProducerAndVersion(t *testing.T) {
	path := "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release"
	files := selectedControlTestFiles{files: map[string]testKbuildVirtualFile{
		path: {content: "same exact bytes\n", exact: true},
	}}
	artifact := KbuildControlReadArtifact{
		Tree: CompactKbuildInvocationObjectTree, Identity: "selected:root:release",
		Version: "exact-version-v1",
		Producer: CompactKbuildVisibleArtifact{
			Path: "include/config/kernel.release", Profile: "root", Target: "release",
		},
	}
	current := artifact
	frontier := selectedControlTestFrontier("frozen-after-writer", files, artifact)
	frontier.ResolveArtifact = func(string) (KbuildControlReadArtifact, bool, error) {
		return current, true, nil
	}
	view := &kbuildControlRecipeReadView{frontier: frontier}
	if _, exists, exact, err := view.Read(path); err != nil || !exists || !exact {
		t.Fatalf("first frozen exact read = %v, %v, %v", exists, exact, err)
	}
	firstIdentity := (&KbuildSelectedControlRecipeSnapshot{view: view}).ReadIdentity()
	for _, tt := range []struct {
		name, want string
		change     func(*KbuildControlReadArtifact)
	}{
		{"changed exact version", "conflicting selected producer/version provenance", func(value *KbuildControlReadArtifact) {
			value.Version = "exact-version-v2"
		}},
		{"same identity different selected writer", "conflicting selected producer/version provenance", func(value *KbuildControlReadArtifact) {
			value.Producer.Target = "sibling-release"
		}},
		{"same selected writer different identity", "conflicting selected producer/version provenance", func(value *KbuildControlReadArtifact) {
			value.Identity = "selected:wrong-owner"
		}},
		{"mismatched rooted producer path", "conflicts with its rooted alias", func(value *KbuildControlReadArtifact) {
			value.Producer.Path = "include/config/sibling.release"
		}},
		{"incomplete selected producer", "incomplete or noncanonical selected producer provenance", func(value *KbuildControlReadArtifact) {
			value.Producer.Target = ""
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			current = artifact
			tt.change(&current)
			if _, _, _, err := view.Read(path); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("inconsistent frozen selected owner read error = %v, want %q", err, tt.want)
			}
			if got := view.readsSnapshot(); len(got) != 1 || got[0].Artifact != artifact ||
				(&KbuildSelectedControlRecipeSnapshot{view: view}).ReadIdentity() != firstIdentity {
				t.Fatalf("failed read changed previously authenticated provenance: %#v", got)
			}
		})
	}
	other := artifact
	other.Producer.Target = "sibling-release"
	independent := &kbuildControlRecipeReadView{frontier: selectedControlTestFrontier("independent-selected-writer", files, other)}
	if _, _, _, err := independent.Read(path); err != nil {
		t.Fatal(err)
	}
	if got := (&KbuildSelectedControlRecipeSnapshot{view: independent}).ReadIdentity(); got == firstIdentity {
		t.Fatalf("identical bytes/version from distinct typed producers shared read identity %q", got)
	}
}

func TestSelectedKbuildControlStepperSourceAndKconfigBaselineHaveNoSelectedProducer(t *testing.T) {
	physical := &kbuildControlRecipeReadView{frontier: selectedControlTestFrontier("declared-source", selectedControlTestFiles{}, KbuildControlReadArtifact{})}
	if err := physical.observePhysicalSource("__LINUX_BZL_SOURCE_TREE__/Makefile", "source bytes\n", true); err != nil {
		t.Fatal(err)
	}
	if got := physical.readsSnapshot(); len(got) != 1 || got[0].Artifact.Producer != (CompactKbuildVisibleArtifact{}) ||
		got[0].Artifact.Tree != CompactKbuildInvocationSourceTree {
		t.Fatalf("declared immutable source owner unexpectedly selected an action: %#v", got)
	}
	path := "__LINUX_BZL_OBJECT_TREE__/include/config/auto.conf"
	baseline := KbuildControlReadArtifact{
		Tree: CompactKbuildInvocationObjectTree, Identity: "resolved-Kconfig:auto.conf", Version: "config-version-v1",
	}
	config := &kbuildControlRecipeReadView{frontier: selectedControlTestFrontier("authenticated-Kconfig-baseline", selectedControlTestFiles{
		files: map[string]testKbuildVirtualFile{path: {content: "CONFIG_TEST=y\n", exact: true}},
	}, baseline)}
	if _, exists, exact, err := config.Read(path); err != nil || !exists || !exact {
		t.Fatalf("authenticated Kconfig baseline = %v, %v, %v", exists, exact, err)
	}
	if got := config.readsSnapshot(); len(got) != 1 || got[0].Artifact != baseline ||
		got[0].Artifact.Producer != (CompactKbuildVisibleArtifact{}) {
		t.Fatalf("Kconfig baseline unexpectedly labeled a selected Kbuild writer: %#v", got)
	}
}

func TestSelectedKbuildControlStepperImmutableSourceAbsenceSharesFrontiers(t *testing.T) {
	var identities []string
	for _, id := range []string{"before-object-action", "after-unrelated-object-action"} {
		view := &kbuildControlRecipeReadView{frontier: selectedControlTestFrontier(
			id, selectedControlTestFiles{}, KbuildControlReadArtifact{})}
		if err := view.observePhysicalSource("__LINUX_BZL_SOURCE_TREE__/missing-input", "", false); err != nil {
			t.Fatal(err)
		}
		identities = append(identities, (&KbuildSelectedControlRecipeSnapshot{view: view}).ReadIdentity())
	}
	if identities[0] == "" || identities[0] != identities[1] {
		t.Fatalf("immutable absent source identity changed across object frontiers: %#v", identities)
	}
}

func TestSelectedKbuildControlStepperEvalAllocatesNewVariableGeneration(t *testing.T) {
	profile, _, _ := selectedControlTestProfile(t, `
export MODE := before
all:
	@echo $(MODE)
	$(eval export MODE := after)
	$(eval INHERITED := file)
	@echo $(MODE)
`)
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	index := selectedControlTestRuleIndex(t, profile, "all")
	frontier := selectedControlTestFrontier("unchanged-no-file-read", selectedControlTestFiles{}, KbuildControlReadArtifact{})
	first, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{Target: "all", RuleIndex: index}, frontier)
	if err != nil {
		t.Fatal(err)
	}
	if first.Environment["MODE"] != "before" {
		t.Fatalf("before first line environment = %#v", first.Environment)
	}
	if err := stepper.ApplyRecipe(first); err != nil {
		t.Fatal(err)
	}
	mutate, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{Target: "all", RuleIndex: index, RecipeIndex: 1}, frontier)
	if err != nil {
		t.Fatal(err)
	}
	if err := stepper.ApplyRecipe(mutate); err != nil {
		t.Fatal(err)
	}
	origin, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{Target: "all", RuleIndex: index, RecipeIndex: 2}, frontier)
	if err != nil {
		t.Fatal(err)
	}
	if err := stepper.ApplyRecipe(origin); err != nil {
		t.Fatal(err)
	}
	last, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{Target: "all", RuleIndex: index, RecipeIndex: 3}, frontier)
	if err != nil {
		t.Fatal(err)
	}
	if last.Environment["MODE"] != "after" {
		t.Fatalf("after source eval environment = %#v", last.Environment)
	}
	if err := stepper.ApplyRecipe(last); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		line *KbuildSelectedControlRecipeSnapshot
		want string
	}{
		{first, "before"}, {mutate, "before"}, {origin, "after"}, {last, "after"},
	} {
		values, err := EvaluateCompactKbuildTarget(tt.line.Evaluation.Profile, "all", "", nil, nil, nil, "MODE")
		if err != nil || values["MODE"] != tt.want {
			t.Fatalf("source generation %d MODE = %#v, %v, want %q", tt.line.Line.RecipeIndex, values, err, tt.want)
		}
	}
	for _, tt := range []struct {
		line *KbuildSelectedControlRecipeSnapshot
		want string
	}{{first, "environment"}, {last, "file"}} {
		actual, err := EvaluateCompactKbuildText(tt.line.Evaluation.Profile, "all", "", nil, nil, nil, "$(origin INHERITED)")
		if err != nil || actual != tt.want {
			t.Fatalf("source generation %d INHERITED origin = %q, %v, want %q", tt.line.Line.RecipeIndex, actual, err, tt.want)
		}
	}
	if first.Evaluation.Profile.evaluator.template.baseVars == nil ||
		last.Evaluation.Profile.evaluator.template.baseVars == nil {
		t.Fatal("per-line evaluators did not share immutable generation variable bases")
	}
	evaluation, err := stepper.Finish(frontier)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ExportedKbuildControlVariables(evaluation); err != nil || got["MODE"] != "after" {
		t.Fatalf("post-eval exported MODE = %#v, %v", got, err)
	}
	if _, err := EvaluateCompactKbuildTarget(evaluation.Profile, "all", "", nil, nil, nil, "MODE"); err == nil || !strings.Contains(err.Error(), "different control state") {
		t.Fatalf("ambiguous target-wide control generation error = %v", err)
	}
}

func TestSelectedKbuildControlStepperInheritsFirstReachedTargetScope(t *testing.T) {
	profile, _, _ := selectedControlTestProfile(t, `
all: MODE := first-parent
all: private HIDDEN := only-parent
all: child
later: MODE := later-parent
later: child
child:
	@echo $(MODE) $(HIDDEN)
`)
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct{ name, parent string }{
		{"all", ""}, {"child", "all"}, {"later", ""}, {"child", "later"},
	} {
		if _, err := stepper.BeginTarget(target.name, target.name, target.parent); err != nil {
			t.Fatalf("begin target %q via %q: %v", target.name, target.parent, err)
		}
	}
	line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
		Target: "child", RuleIndex: selectedControlTestRuleIndex(t, profile, "child"),
	}, selectedControlTestFrontier("no-file-read", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	values, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "child", "", nil, nil, nil, "MODE", "HIDDEN")
	if err != nil || values["MODE"] != "first-parent" || values["HIDDEN"] != "" {
		t.Fatalf("first-reached inherited/non-private target scope = %#v, %v", values, err)
	}
}

func TestSelectedKbuildRuleSecondExpansionUsesFirstSourceRecipeFrontier(t *testing.T) {
	profile, _, _ := selectedControlTestProfile(t, `
PREREQ = $(if $(wildcard include/config/kernel.release),late.c,early.c)
.SECONDEXPANSION:
result.o: $$(PREREQ)
	@echo before
	@printf release > include/config/kernel.release
	@echo after
`)
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("result.o", "result.o", ""); err != nil {
		t.Fatal(err)
	}
	index := selectedControlTestRuleIndex(t, profile, "result.o")
	path := "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release"
	files := selectedControlTestFiles{
		files:   map[string]testKbuildVirtualFile{path: {content: "release\n", exact: true}},
		matches: map[string][]string{path: {path}},
	}
	frontiers := []KbuildControlRecipeFrontier{
		selectedControlTestFrontier("rule-entry", selectedControlTestFiles{}, KbuildControlReadArtifact{}),
		selectedControlTestFrontier("writer-line", selectedControlTestFiles{}, KbuildControlReadArtifact{}),
		selectedControlTestFrontier("after-writer", files, KbuildControlReadArtifact{
			Tree: CompactKbuildInvocationObjectTree, Identity: "source-selected:release",
			Version: "release-bytes", Producer: CompactKbuildVisibleArtifact{
				Path: "include/config/kernel.release", Profile: "root", Target: "result.o",
			},
		}),
	}
	for recipeIndex, frontier := range frontiers {
		line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
			Target: "result.o", LookupTarget: "result.o", RuleIndex: index, RecipeIndex: recipeIndex,
		}, frontier)
		if err != nil {
			t.Fatalf("source recipe %d snapshot: %v", recipeIndex, err)
		}
		if recipeIndex != 1 {
			values, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "result.o", "", nil, nil, nil, "PREREQ")
			want := "early.c"
			if recipeIndex == 2 {
				want = "late.c"
			}
			if err != nil || values["PREREQ"] != want {
				t.Fatalf("source recipe %d prereq = %#v, %v; want %q", recipeIndex, values, err, want)
			}
		}
		if err := stepper.ApplyRecipe(line); err != nil {
			t.Fatalf("apply recipe %d: %v", recipeIndex, err)
		}
	}
	evaluation, err := stepper.Finish(frontiers[2])
	if err != nil {
		t.Fatal(err)
	}
	context, err := evaluatedKbuildSelectedTargetMakeContext(evaluation.Profile, "result.o", nil)
	if err != nil || len(context.normal) != 1 || context.normal[0].graphPath != "early.c" {
		t.Fatalf("source-selected second expansion = %#v, %v; want first recipe early.c", context.normal, err)
	}
	if _, err := EvaluateCompactKbuildTarget(evaluation.Profile, "result.o", "", nil, nil, nil, "PREREQ"); err == nil ||
		!strings.Contains(err.Error(), "different file reads") {
		t.Fatalf("aggregate target evaluator accepted distinct source reads: %v", err)
	}
	broken := evaluation.Profile
	first := *evaluation.Profile.targetRuleEntrySnapshots["result.o"]
	first.Line.RuleIndex = -1
	broken.targetRuleEntrySnapshots = map[string]*KbuildSelectedControlRuleEntry{"result.o": &first}
	if _, err := evaluatedKbuildSelectedTargetMakeContext(broken, "result.o", nil); err == nil ||
		!strings.Contains(err.Error(), "no matching source-selected rule entry snapshot") {
		t.Fatalf("mismatched source-selected second expansion authority: %v", err)
	}
}

func TestSelectedKbuildTargetStemUsesSourceRuleEntryBeforeLaterRecipeReads(t *testing.T) {
	profile, _, _ := selectedControlTestProfile(t, `
target-stem = $(if $(file < include/config/kernel.release),after,before)
all:
	@echo $(target-stem)
	@printf release > include/config/kernel.release
	@echo $(target-stem)
`)
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	index := selectedControlTestRuleIndex(t, profile, "all")
	path := "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release"
	produced := KbuildControlReadArtifact{
		Tree: CompactKbuildInvocationObjectTree, Identity: "source-selected:release", Version: "writer-v1",
		Producer: CompactKbuildVisibleArtifact{Path: "include/config/kernel.release", Profile: "root", Target: "all"},
	}
	frontiers := []KbuildControlRecipeFrontier{
		selectedControlTestFrontier("entry-before-writer", selectedControlTestFiles{}, produced),
		selectedControlTestFrontier("writer-line", selectedControlTestFiles{}, produced),
		selectedControlTestFrontier("later-after-writer", selectedControlTestFiles{
			files: map[string]testKbuildVirtualFile{path: {content: "release\n", exact: true}},
		}, produced),
	}
	for recipeIndex, frontier := range frontiers {
		line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
			Target: "all", LookupTarget: "all", RuleIndex: index, RecipeIndex: recipeIndex,
		}, frontier)
		if err != nil {
			t.Fatalf("source recipe %d: %v", recipeIndex, err)
		}
		if recipeIndex != 1 {
			values, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "all", "", nil, nil, nil, "target-stem")
			want := "before"
			if recipeIndex == 2 {
				want = "after"
			}
			if err != nil || values["target-stem"] != want {
				t.Fatalf("source recipe %d target-stem = %#v, %v; want %q", recipeIndex, values, err, want)
			}
		}
		if err := stepper.ApplyRecipe(line); err != nil {
			t.Fatalf("apply recipe %d: %v", recipeIndex, err)
		}
	}
	evaluation, err := stepper.Finish(frontiers[2])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EvaluateCompactKbuildTarget(evaluation.Profile, "all", "", nil, nil, nil, "target-stem"); err == nil ||
		!strings.Contains(err.Error(), "different file reads") {
		t.Fatalf("target-wide target-stem read accepted distinct source file versions: %v", err)
	}
	injections, err := CompactKbuildTargetEvaluationInjectionsForMakeTarget(evaluation.Profile, "all", "all", "all", "", nil, nil)
	if err != nil || injections["target-stem"] != "before" {
		t.Fatalf("source rule-entry target-stem = %#v, %v; want before", injections, err)
	}
	if _, err := CompactKbuildTargetEvaluationInjectionsForMakeTarget(evaluation.Profile, "all", "other", "all", "", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "no matching source-selected rule entry") {
		t.Fatalf("unmatched lexical rule lookup accepted first source snapshot: %v", err)
	}
	broken := evaluation.Profile
	first := *evaluation.Profile.targetRuleEntrySnapshots["all"]
	first.Line.RuleIndex = -1
	broken.targetRuleEntrySnapshots = map[string]*KbuildSelectedControlRuleEntry{"all": &first}
	if _, err := CompactKbuildTargetEvaluationInjectionsForMakeTarget(broken, "all", "all", "all", "", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "no matching source-selected rule entry") {
		t.Fatalf("invalid rule entry accepted target-stem lookup: %v", err)
	}
}

func TestSelectedKbuildControlStepperVirtualObjectWildcardTracksWriterAndMembership(t *testing.T) {
	profile, _, objectRoot := selectedControlTestProfile(t, `
SELECTED = $(wildcard include/config/*.release)
all:
	@echo $(SELECTED)
	@echo writer
	@echo $(SELECTED)
`)
	physical := filepath.Join(objectRoot, "include", "config", "kernel.release")
	if err := os.WriteFile(physical, []byte("stale-object-wildcard\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(physical); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("stale physical wildcard Lstat = %v, %v", info, err)
	}
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	index := selectedControlTestRuleIndex(t, profile, "all")
	pattern := "__LINUX_BZL_OBJECT_TREE__/include/config/*.release"
	path := "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release"
	artifact := KbuildControlReadArtifact{
		Tree: CompactKbuildInvocationObjectTree, Identity: "root/all/kernel.release", Version: "writer-v1",
		Producer: CompactKbuildVisibleArtifact{
			Path: "include/config/kernel.release", Profile: "root", Target: "all",
		},
	}
	frontiers := []KbuildControlRecipeFrontier{
		selectedControlTestFrontier("before-writer", selectedControlTestFiles{}, artifact),
		selectedControlTestFrontier("writer-line", selectedControlTestFiles{}, artifact),
		selectedControlTestFrontier("after-writer", selectedControlTestFiles{
			files:   map[string]testKbuildVirtualFile{path: {content: "6.18.39-test\n", exact: true}},
			matches: map[string][]string{pattern: {path}},
		}, artifact),
	}
	var before, after *KbuildSelectedControlRecipeSnapshot
	for recipeIndex, frontier := range frontiers {
		line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
			Target: "all", RuleIndex: index, RecipeIndex: recipeIndex,
		}, frontier)
		if err != nil {
			t.Fatalf("before wildcard recipe %d: %v", recipeIndex, err)
		}
		if recipeIndex != 1 {
			values, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "all", "", nil, nil, nil, "SELECTED")
			want := ""
			if recipeIndex == 2 {
				want = "include/config/kernel.release"
			}
			if err != nil || values["SELECTED"] != want {
				t.Fatalf("recipe %d Make wildcard = %#v, %v; want %q", recipeIndex, values, err, want)
			}
		}
		if err := stepper.ApplyRecipe(line); err != nil {
			t.Fatalf("apply wildcard recipe %d: %v", recipeIndex, err)
		}
		if recipeIndex == 0 {
			before = line
		}
		if recipeIndex == 2 {
			after = line
		}
	}
	if got := before.Reads(); len(got) != 1 || !got[0].Wildcard || got[0].Path != pattern ||
		got[0].Exists || got[0].FrontierID != "before-writer" {
		t.Fatalf("pre-writer wildcard absence = %#v", got)
	}
	if got := after.Reads(); len(got) != 2 || !got[0].Wildcard || got[0].Path != path ||
		got[0].Artifact != artifact || !got[1].Wildcard || got[1].Path != pattern ||
		!got[1].Exists || got[1].MembershipVersion == "" || got[1].FrontierID != "" {
		t.Fatalf("post-writer exact matched producer and pattern membership = %#v", got)
	}
	if before.ReadIdentity() == after.ReadIdentity() || before.ReadIdentity() == "" {
		t.Fatalf("wildcard writer did not change read identity: %q, %q", before.ReadIdentity(), after.ReadIdentity())
	}
	if _, err := stepper.Finish(frontiers[2]); err != nil {
		t.Fatal(err)
	}
	other := &kbuildControlRecipeReadView{frontier: selectedControlTestFrontier("after-unrelated-step", selectedControlTestFiles{
		files:   map[string]testKbuildVirtualFile{path: {content: "6.18.39-test\n", exact: true}},
		matches: map[string][]string{pattern: {path}},
	}, artifact)}
	if _, err := other.MatchRead(pattern); err != nil {
		t.Fatal(err)
	}
	if actual := (&KbuildSelectedControlRecipeSnapshot{view: other}).ReadIdentity(); actual != after.ReadIdentity() {
		t.Fatalf("same wildcard producer/membership after unrelated frontier changed identity %q vs %q", actual, after.ReadIdentity())
	}
}

func TestSelectedKbuildControlStepperRejectsOpaqueAndAmbiguousObjectWildcard(t *testing.T) {
	profile, _, _ := selectedControlTestProfile(t, `
SELECTED = $(wildcard include/config/*.release)
all:
	@echo $(SELECTED)
`)
	pattern := "__LINUX_BZL_OBJECT_TREE__/include/config/*.release"
	path := "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release"
	for _, tt := range []struct {
		name, want string
		frontier   KbuildControlRecipeFrontier
	}{
		{name: "opaque match", want: "absent or opaque contents", frontier: selectedControlTestFrontier("opaque", selectedControlTestFiles{
			files: map[string]testKbuildVirtualFile{path: {exact: false}}, matches: map[string][]string{pattern: {path}},
		}, KbuildControlReadArtifact{})},
		{name: "unowned match alias", want: "no object-tree owner", frontier: selectedControlTestFrontier("alias", selectedControlTestFiles{
			matches: map[string][]string{pattern: {"__LINUX_BZL_SOURCE_TREE__/include/config/kernel.release"}},
		}, KbuildControlReadArtifact{})},
		{name: "ambiguous owner", want: "distinct sibling producers", frontier: KbuildControlRecipeFrontier{
			ID: "sibling-conflict", Files: selectedControlTestFiles{
				files:   map[string]testKbuildVirtualFile{path: {content: "same\n", exact: true}},
				matches: map[string][]string{pattern: {path}},
			}, ResolveArtifact: func(string) (KbuildControlReadArtifact, bool, error) {
				return KbuildControlReadArtifact{}, false, errors.New("distinct sibling producers for one matched alias")
			},
		}},
		{name: "same identity conflicting exact matched producers", want: "conflicting selected producer/version provenance", frontier: KbuildControlRecipeFrontier{
			ID: "matched-owner-conflict", Files: selectedControlTestFiles{
				files: map[string]testKbuildVirtualFile{
					path: {content: "same\n", exact: true},
					"__LINUX_BZL_OBJECT_TREE__/include/config/sibling.release": {content: "same\n", exact: true},
				},
				matches: map[string][]string{pattern: {path, "__LINUX_BZL_OBJECT_TREE__/include/config/sibling.release"}},
			}, ResolveArtifact: func(logical string) (KbuildControlReadArtifact, bool, error) {
				return KbuildControlReadArtifact{
					Tree: CompactKbuildInvocationObjectTree, Identity: "selected:shared-owner", Version: "same-version",
					Producer: CompactKbuildVisibleArtifact{
						Path: strings.TrimPrefix(logical, "__LINUX_BZL_OBJECT_TREE__/"), Profile: "root", Target: "shared",
					},
				}, true, nil
			},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
				t.Fatal(err)
			}
			line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
				Target: "all", RuleIndex: selectedControlTestRuleIndex(t, profile, "all"),
			}, tt.frontier)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "all", "", nil, nil, nil, "SELECTED"); err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "Makefile:") {
				t.Fatalf("selected %s wildcard error = %v, want source-located %q", tt.name, err, tt.want)
			}
			if got := line.Reads(); len(got) != 0 {
				t.Fatalf("failed ambiguous/opaque wildcard recorded partially resolved reads: %#v", got)
			}
		})
	}
}

func TestSelectedKbuildControlStepperExactOpaqueObjectWildcardTracksSelectedPresence(t *testing.T) {
	path := "__LINUX_BZL_OBJECT_TREE__/generated/fixdep.o"
	glob := "__LINUX_BZL_OBJECT_TREE__/generated/*.o"
	files := selectedControlTestFiles{
		files:   map[string]testKbuildVirtualFile{path: {exact: false}},
		matches: map[string][]string{path: {path}, glob: {path}},
	}
	producer := CompactKbuildVisibleArtifact{
		Path: "generated/fixdep.o", Profile: "build:fixdep:first", Target: "generated/fixdep.o",
	}
	frontier := selectedControlTestFrontier("completed-first-child", files, KbuildControlReadArtifact{})
	frontier.ResolvePresenceArtifact = func(string) (KbuildControlReadArtifact, bool, error) {
		return KbuildControlReadArtifact{
			Tree: CompactKbuildInvocationObjectTree, Identity: "presence:first-child:fixdep.o",
			Version: "completed-first-child", Producer: producer,
		}, true, nil
	}
	view := &kbuildControlRecipeReadView{frontier: frontier}
	if got, err := view.MatchRead(path); err != nil || len(got) != 1 || got[0] != path {
		t.Fatalf("literal opaque object presence = %#v, %v; want exact path", got, err)
	}
	reads := view.readsSnapshot()
	if len(reads) != 2 || reads[0].Artifact.Producer != producer || !reads[0].Wildcard ||
		!reads[1].Wildcard || reads[1].MembershipVersion == "" {
		t.Fatalf("literal opaque object producer/membership = %#v", reads)
	}
	if _, _, _, err := view.Read(path); err == nil || !strings.Contains(err.Error(), "opaque contents") {
		t.Fatalf("literal presence became a byte read: %v", err)
	}
	for _, test := range []struct {
		name, pattern, want string
		alter               func(*KbuildControlRecipeFrontier)
	}{
		{name: "opaque glob", pattern: glob, want: "absent or opaque contents"},
		{name: "missing typed producer", pattern: path, want: "no artifact owner resolver", alter: func(f *KbuildControlRecipeFrontier) {
			f.ResolvePresenceArtifact = nil
		}},
		{name: "ambiguous typed producer", pattern: path, want: "distinct sibling producers", alter: func(f *KbuildControlRecipeFrontier) {
			f.ResolvePresenceArtifact = func(string) (KbuildControlReadArtifact, bool, error) {
				return KbuildControlReadArtifact{}, false, errors.New("distinct sibling producers")
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			frozen := frontier
			if test.alter != nil {
				test.alter(&frozen)
			}
			negative := &kbuildControlRecipeReadView{frontier: frozen}
			if _, err := negative.MatchRead(test.pattern); err == nil || !strings.Contains(err.Error(), test.want) || len(negative.readsSnapshot()) != 0 {
				t.Fatalf("unknown membership/producer error = %v, recorded %#v; want %q", err, negative.readsSnapshot(), test.want)
			}
		})
	}
}

func TestSelectedKbuildControlStepperFinalLazyExportsReadCompletionFrontier(t *testing.T) {
	profile, _, objectRoot := selectedControlTestProfile(t, `
export KERNELRELEASE = $(file < include/config/kernel.release)
all:
	@echo writer
`)
	physical := filepath.Join(objectRoot, "include", "config", "kernel.release")
	if err := os.WriteFile(physical, []byte("stale-before-writer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	before, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
		Target: "all", RuleIndex: selectedControlTestRuleIndex(t, profile, "all"),
	}, selectedControlTestFrontier("before-writer", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := stepper.ApplyRecipe(before); err != nil {
		t.Fatal(err)
	}
	path := "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release"
	artifact := KbuildControlReadArtifact{
		Tree: CompactKbuildInvocationObjectTree, Identity: "root/all/release writer", Version: "release-v1",
	}
	completion := selectedControlTestFrontier("completion-after-writer", selectedControlTestFiles{
		files: map[string]testKbuildVirtualFile{path: {content: "6.18.39-test\n", exact: true}},
	}, artifact)
	evaluation, err := stepper.Finish(completion)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := ExportedKbuildControlVariables(evaluation)
	if err != nil || exported["KERNELRELEASE"] != "6.18.39-test" {
		t.Fatalf("completion exported KERNELRELEASE = %#v, %v", exported, err)
	}
	if got := evaluation.Profile.evaluator.template.virtualFileView.(*kbuildControlRecipeReadView).readsSnapshot(); len(got) != 1 || got[0].Path != path ||
		!got[0].Exists || got[0].Artifact != artifact {
		t.Fatalf("final exported Make read provenance = %#v", got)
	}
	if got := before.Reads(); len(got) != 1 || got[0].Exists || got[0].FrontierID != "before-writer" {
		t.Fatalf("earlier recipe export read was retroactively changed: %#v", got)
	}
	missingOwner := selectedControlTestFrontier("missing-owner", selectedControlTestFiles{
		files: map[string]testKbuildVirtualFile{path: {content: "6.18.39-test\n", exact: true}},
	}, KbuildControlReadArtifact{})
	stepper2, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	final, err := stepper2.Finish(missingOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExportedKbuildControlVariables(final); err == nil || !strings.Contains(err.Error(), "unknown artifact") {
		t.Fatalf("unowned final export error = %v, want unknown owner", err)
	}
}

func TestSelectedKbuildControlStepperDoesNotCachePreWriterAbsence(t *testing.T) {
	profile, _, _ := selectedControlTestProfile(t, `
KERNELRELEASE = $(shell cat include/config/kernel.release 2>/dev/null)
all:
	@echo writer
	@echo $(KERNELRELEASE)
`)
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	index := selectedControlTestRuleIndex(t, profile, "all")
	path := "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release"
	artifact := KbuildControlReadArtifact{
		Tree: CompactKbuildInvocationObjectTree, Identity: "root/all/kernel.release", Version: "release-v1",
	}
	writer, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{Target: "all", RuleIndex: index},
		selectedControlTestFrontier("before", selectedControlTestFiles{}, artifact))
	if err != nil {
		t.Fatal(err)
	}
	if got := writer.ReadIdentity(); got != "" {
		t.Fatalf("nonreading writer line identity = %q, want empty", got)
	}
	if err := stepper.ApplyRecipe(writer); err != nil {
		t.Fatal(err)
	}
	reader, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{Target: "all", RuleIndex: index, RecipeIndex: 1},
		selectedControlTestFrontier("after", selectedControlTestFiles{files: map[string]testKbuildVirtualFile{
			path: {content: "6.18.39-test\n", exact: true},
		}}, artifact))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		values, err := EvaluateCompactKbuildTarget(reader.Evaluation.Profile, "all", "", nil, nil, nil, "KERNELRELEASE")
		if err != nil || values["KERNELRELEASE"] != "6.18.39-test" {
			t.Fatalf("reader expansion %d = %#v, %v", i, values, err)
		}
	}
	if got := reader.Reads(); len(got) != 2 || got[0].Path != path || got[1].Path != path || !got[0].Exists {
		t.Fatalf("reader sourced optional cat exact twice = %#v", got)
	}
	if err := stepper.ApplyRecipe(reader); err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.Finish(selectedControlTestFrontier("after", selectedControlTestFiles{
		files: map[string]testKbuildVirtualFile{path: {content: "6.18.39-test\n", exact: true}},
	}, artifact)); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedKbuildControlStepperRejectsOpaqueAndAmbiguousObjectReads(t *testing.T) {
	profile, _, _ := selectedControlTestProfile(t, `
KERNELRELEASE = $(file < include/config/kernel.release)
all:
	@echo $(KERNELRELEASE)
`)
	path := "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release"
	for _, tt := range []struct {
		name     string
		frontier KbuildControlRecipeFrontier
		want     string
	}{
		{name: "opaque", frontier: selectedControlTestFrontier("opaque", selectedControlTestFiles{
			files: map[string]testKbuildVirtualFile{path: {exact: false}},
		}, KbuildControlReadArtifact{}), want: "opaque contents"},
		{name: "ambiguous owners", frontier: KbuildControlRecipeFrontier{
			ID: "sibling-join", Files: selectedControlTestFiles{files: map[string]testKbuildVirtualFile{
				path: {content: "same bytes\n", exact: true},
			}}, ResolveArtifact: func(string) (KbuildControlReadArtifact, bool, error) {
				return KbuildControlReadArtifact{}, false, errors.New("sibling actions have distinct producers for same-byte aliases")
			},
		}, want: "distinct producers"},
		{name: "unknown owner", frontier: KbuildControlRecipeFrontier{
			ID: "unknown", Files: selectedControlTestFiles{files: map[string]testKbuildVirtualFile{
				path: {content: "same bytes\n", exact: true},
			}},
		}, want: "no artifact owner resolver"},
		{name: "wrong tree", frontier: selectedControlTestFrontier("wrong-tree", selectedControlTestFiles{
			files: map[string]testKbuildVirtualFile{path: {content: "same bytes\n", exact: true}},
		}, KbuildControlReadArtifact{Tree: CompactKbuildInvocationSourceTree, Identity: "source/foo", Version: "v1"}), want: "resolves to"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
				t.Fatal(err)
			}
			line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
				Target: "all", RuleIndex: selectedControlTestRuleIndex(t, profile, "all"),
			}, tt.frontier)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "all", "", nil, nil, nil, "KERNELRELEASE"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("%s virtual read error = %v, want %q", tt.name, err, tt.want)
			} else if !strings.Contains(err.Error(), "Makefile:") {
				t.Fatalf("%s file read error lacks source location: %v", tt.name, err)
			}
		})
	}
}

func TestSelectedKbuildControlStepperSourceAndObjectRelativeReadsUseTypedCwd(t *testing.T) {
	profile, sourceRoot, objectRoot := selectedControlTestProfile(t, `
KERNELRELEASE = $(file < include/config/kernel.release)
all:
	@echo $(KERNELRELEASE)
`)
	for _, fixture := range []struct{ root, contents string }{
		{sourceRoot, "immutable-source\n"},
		{objectRoot, "stale-object-host\n"},
	} {
		if err := os.WriteFile(filepath.Join(fixture.root, "include", "config", "kernel.release"), []byte(fixture.contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name, want, treePath string
		location             CompactKbuildInvocationLocation
		frontier             KbuildControlRecipeFrontier
	}{
		{name: "object absent", want: "", treePath: "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release",
			location: CompactKbuildInvocationLocation{Tree: CompactKbuildInvocationObjectTree},
			frontier: selectedControlTestFrontier("no-object-writer", selectedControlTestFiles{}, KbuildControlReadArtifact{})},
		{name: "source physical", want: "immutable-source", treePath: "__LINUX_BZL_SOURCE_TREE__/include/config/kernel.release",
			location: CompactKbuildInvocationLocation{Tree: CompactKbuildInvocationSourceTree},
			frontier: selectedControlTestFrontier("same-immutable-source", selectedControlTestFiles{}, KbuildControlReadArtifact{})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			selected := profile
			if err := SetCompactKbuildProfileInvocationLocation(&selected, tt.location); err != nil {
				t.Fatal(err)
			}
			stepper, err := NewSelectedKbuildControlStepper(selected, KbuildControlEvaluationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
				t.Fatal(err)
			}
			line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
				Target: "all", RuleIndex: selectedControlTestRuleIndex(t, selected, "all"),
			}, tt.frontier)
			if err != nil {
				t.Fatal(err)
			}
			values, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "all", "", nil, nil, nil, "KERNELRELEASE")
			if err != nil || values["KERNELRELEASE"] != tt.want {
				t.Fatalf("%s value = %#v, %v, want %q", tt.name, values, err, tt.want)
			}
			reads := line.Reads()
			if len(reads) != 1 || reads[0].Path != tt.treePath || reads[0].Exists != (tt.want != "") {
				t.Fatalf("%s typed source/object read = %#v", tt.name, reads)
			}
			if tt.want != "" && reads[0].Artifact.Tree != CompactKbuildInvocationSourceTree {
				t.Fatalf("source-root producer = %#v, want declared source", reads[0].Artifact)
			}
		})
	}
}

func TestSelectedKbuildControlStepperRelativeReadCanTraverseWithinDeclaredTree(t *testing.T) {
	profile, sourceRoot, objectRoot := selectedControlTestProfile(t, `
KERNELRELEASE = $(file < ../include/config/kernel.release)
all:
	@echo $(KERNELRELEASE)
`)
	for _, fixture := range []struct{ root, contents string }{
		{sourceRoot, "source-release\n"},
		{objectRoot, "host-only-release\n"},
	} {
		if err := os.WriteFile(filepath.Join(fixture.root, "include", "config", "kernel.release"), []byte(fixture.contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name, want, path string
		location         CompactKbuildInvocationLocation
	}{
		{name: "source", want: "source-release", path: "__LINUX_BZL_SOURCE_TREE__/include/config/kernel.release",
			location: CompactKbuildInvocationLocation{Tree: CompactKbuildInvocationSourceTree, Directory: "sub"}},
		{name: "object", want: "", path: "__LINUX_BZL_OBJECT_TREE__/include/config/kernel.release",
			location: CompactKbuildInvocationLocation{Tree: CompactKbuildInvocationObjectTree, Directory: "sub"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			selected := profile
			if err := SetCompactKbuildProfileInvocationLocation(&selected, tt.location); err != nil {
				t.Fatal(err)
			}
			stepper, err := NewSelectedKbuildControlStepper(selected, KbuildControlEvaluationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stepper.BeginTarget("sub/all", "all", ""); err != nil {
				t.Fatal(err)
			}
			line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
				Target: "sub/all", RuleIndex: selectedControlTestRuleIndex(t, selected, "all"),
			}, selectedControlTestFrontier("declared-tree", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
			if err != nil {
				t.Fatal(err)
			}
			values, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "sub/all", "", nil, nil, nil, "KERNELRELEASE")
			if err != nil || values["KERNELRELEASE"] != tt.want {
				t.Fatalf("within-tree traversal = %#v, %v, want %q", values, err, tt.want)
			}
			if got := line.Reads(); len(got) != 1 || got[0].Path != tt.path || got[0].Exists != (tt.want != "") {
				t.Fatalf("typed within-tree path/read = %#v, want %q", got, tt.path)
			}
		})
	}
}

func TestSelectedKbuildControlStepperRejectsUnownedPhysicalAbsoluteRead(t *testing.T) {
	physical := filepath.Join(t.TempDir(), "outside-the-declared-roots")
	if err := os.WriteFile(physical, []byte("host-only-release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile, _, _ := selectedControlTestProfile(t, fmt.Sprintf(`
KERNELRELEASE = $(file < %s)
all:
	@echo $(KERNELRELEASE)
`, filepath.ToSlash(physical)))
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
		Target: "all", RuleIndex: selectedControlTestRuleIndex(t, profile, "all"),
	}, selectedControlTestFrontier("no-owned-read", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "all", "", nil, nil, nil, "KERNELRELEASE"); err == nil || !strings.Contains(err.Error(), "no declared immutable source or virtual object owner") ||
		!strings.Contains(err.Error(), "Makefile:") {
		t.Fatalf("physically present unowned absolute read error = %v, want source-located closed rejection", err)
	}
	if got := line.Reads(); len(got) != 0 {
		t.Fatalf("unowned host file was offered to the virtual read logger: %#v", got)
	}
}

func TestSelectedKbuildControlStepperRejectsSourceRootSymlinkEscape(t *testing.T) {
	profile, sourceRoot, _ := selectedControlTestProfile(t, `
KERNELRELEASE = $(file < include/config/kernel.release)
all:
	@echo $(KERNELRELEASE)
`)
	external := filepath.Join(t.TempDir(), "outside-source-root")
	if err := os.WriteFile(external, []byte("host-only-source-bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(sourceRoot, "include", "config", "kernel.release")
	if err := os.Symlink(external, symlink); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(symlink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("external source symlink Lstat = %v, %v", info, err)
	}
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
		Tree: CompactKbuildInvocationSourceTree,
	}); err != nil {
		t.Fatal(err)
	}
	stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stepper.BeginTarget("all", "all", ""); err != nil {
		t.Fatal(err)
	}
	line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
		Target: "all", RuleIndex: selectedControlTestRuleIndex(t, profile, "all"),
	}, selectedControlTestFrontier("source-root", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EvaluateCompactKbuildTarget(line.Evaluation.Profile, "all", "", nil, nil, nil, "KERNELRELEASE"); err == nil || !strings.Contains(err.Error(), "escapes its declared immutable source root") ||
		!strings.Contains(err.Error(), "Makefile:") {
		t.Fatalf("source-root symlink escape error = %v, want source-located closed rejection", err)
	}
	if got := line.Reads(); len(got) != 0 {
		t.Fatalf("escaped source file was treated as an immutable source read: %#v", got)
	}
}

func TestSelectedOrdinaryRecipeDeferredQueryKeepsSourceRegistry(t *testing.T) {
	for _, beforeCompletion := range []bool{true, false} {
		t.Run(fmt.Sprintf("before-completion=%t", beforeCompletion), func(t *testing.T) {
			const target = "tools/lib/bpf/check_abi"
			profile, _, _ := selectedControlTestProfile(t, `
if_changed = $(cmd_$(1))
cmd_check_abi = printf '%s\n' "$(shell readelf -W -s $(objtree)/tools/lib/bpf/libbpf.so)" > $@
all: tools/lib/bpf/check_abi
tools/lib/bpf/check_abi: FORCE
	$(call if_changed,check_abi)
.PHONY: FORCE
FORCE:
`)
			stepper, err := NewSelectedKbuildControlStepper(profile, KbuildControlEvaluationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stepper.BeginTarget(target, target, ""); err != nil {
				t.Fatal(err)
			}
			line, err := stepper.BeforeRecipe(KbuildSelectedControlRecipeLine{
				Target: target, RuleIndex: selectedControlTestRuleIndex(t, profile, target),
			}, selectedControlTestFrontier("before-check-abi", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
			if err != nil {
				t.Fatal(err)
			}
			var first CompactKbuildSelectedTargetEffects
			if beforeCompletion {
				var selected bool
				first, selected, err = EvaluateCompactKbuildSelectedTargetEffects(line.Evaluation.Profile, target)
				if err != nil || !selected || len(first.DeferredContentQueries) != 1 {
					t.Fatalf("selected precompletion source shell query = (%#v, %t, %v), want one deferred query", first, selected, err)
				}
			}
			if err := stepper.ApplyRecipe(line); err != nil {
				t.Fatal(err)
			}
			evaluation, err := stepper.Finish(selectedControlTestFrontier("after-check-abi", selectedControlTestFiles{}, KbuildControlReadArtifact{}))
			if err != nil {
				t.Fatal(err)
			}
			if !beforeCompletion {
				var selected bool
				first, selected, err = EvaluateCompactKbuildSelectedTargetEffects(evaluation.Profile, target)
				if err != nil || !selected || len(first.DeferredContentQueries) != 1 {
					t.Fatalf("selected postcompletion source shell query = (%#v, %t, %v), want one deferred query", first, selected, err)
				}
			}
			query := first.DeferredContentQueries[0]
			if query.Origin != (KbuildDeferredContentOrigin{Profile: profile.Name, Target: target}) {
				t.Fatalf("source query origin = %#v, want exact selected recipe", query.Origin)
			}
			profiles := []CompactKbuildProfile{evaluation.Profile}
			selection := KbuildDeferredContentSelection{
				Token: query.Token, Profile: profile.Name, Target: target, Lifecycle: "target", Scope: "target", Stage: "target",
			}
			if err := ApplyKbuildDeferredContentSelections(profiles, map[string]KbuildDeferredContentSelection{
				query.Token: selection,
			}); err != nil {
				t.Fatalf("bind selected source query to completed profile: %v", err)
			}
			if got := profiles[0].deferredContentQueries[query.Token]; !sameKbuildDeferredContentQuerySource(got, query) || got.Selection != selection {
				t.Fatalf("completed profile query = %#v, want exact selected source query %#v", got, query)
			}
			if err := ApplyKbuildDeferredContentSelections(profiles, map[string]KbuildDeferredContentSelection{
				kbuildDeferredContentTokenPrefix + strings.Repeat("f", 64): {
					Profile: profile.Name, Target: target,
				},
			}); err == nil || !strings.Contains(err.Error(), "has no profile registry") {
				t.Fatalf("unbound selected query error = %v, want closed source registry rejection", err)
			}
		})
	}
}
