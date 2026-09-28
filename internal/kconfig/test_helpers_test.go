package kconfig

import (
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var testConfiguredActionRoles = []string{
	"ar", "as", "awk", "cc", "cxx", "ld", "nm", "objcopy", "objdump",
	"ranlib", "readelf", "strip",
}

func testActionRoleRefs(scopes []string, roles ...string) []KbuildActionRoleRef {
	refs := make([]KbuildActionRoleRef, 0, len(scopes)*len(roles))
	for _, scope := range scopes {
		for _, role := range roles {
			refs = append(refs, KbuildActionRoleRef{Scope: scope, Role: role})
		}
	}
	return refs
}

func testScopedActionRoles(roles ...string) []KbuildActionRoleRef {
	return testActionRoleRefs([]string{"target", "host"}, roles...)
}

func testTargetActionRoles(roles ...string) []KbuildActionRoleRef {
	return testActionRoleRefs([]string{"target"}, roles...)
}

func testHostActionRoles(roles ...string) []KbuildActionRoleRef {
	return testActionRoleRefs([]string{"host"}, roles...)
}

func withMaterializedInitialObjectTreeArtifactForTest(
	t *testing.T,
	builder compactKbuildRulePlanBuilder,
	artifact CompactKbuildVisibleArtifact,
	stage, lifecycle, scope, producer string,
) compactKbuildRulePlanBuilder {
	t.Helper()
	ownerProfile := CompactKbuildProfile{
		Name: artifact.Profile, Path: "Makefile", EntryTargets: []string{artifact.Target},
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{ownerProfile},
		KbuildSelections: []CompactKbuildSelection{{
			Profile: artifact.Profile, Target: artifact.Target, MakeTarget: artifact.Target, Lifecycle: lifecycle, Scope: scope, Stage: stage,
		}},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatalf("build exact visible-artifact selection graph: %v", err)
	}
	key := compactKbuildSelectionKey{profile: artifact.Profile, target: artifact.Target, stage: stage}
	if err := graph.recordMaterializedProducer(key, producer); err != nil {
		t.Fatalf("record exact visible-artifact producer: %v", err)
	}
	return builder.withSelectionGraph(graph).withInitialObjectTree(true, artifact)
}

var testConfiguredScopedActionRoles = testScopedActionRoles(testConfiguredActionRoles...)

type staticCompactKbuildInitialVisibleArtifactView struct {
	artifacts []CompactKbuildVisibleArtifact
}

func (view staticCompactKbuildInitialVisibleArtifactView) Len() int {
	return len(view.artifacts)
}

func (view staticCompactKbuildInitialVisibleArtifactView) Get(path string) (CompactKbuildVisibleArtifact, bool) {
	index := sort.Search(len(view.artifacts), func(index int) bool {
		return view.artifacts[index].Path >= path
	})
	if index == len(view.artifacts) || view.artifacts[index].Path != path {
		return CompactKbuildVisibleArtifact{}, false
	}
	return view.artifacts[index], true
}

func (view staticCompactKbuildInitialVisibleArtifactView) Range(
	prefix string,
	visit func(CompactKbuildVisibleArtifact) bool,
) {
	start := 0
	if prefix != "" {
		start = sort.Search(len(view.artifacts), func(index int) bool {
			return view.artifacts[index].Path >= prefix
		})
	}
	for _, artifact := range view.artifacts[start:] {
		if prefix != "" && !strings.HasPrefix(artifact.Path, prefix) {
			return
		}
		if !visit(artifact) {
			return
		}
	}
}

func setTestCompactKbuildInitialVisibleArtifacts(
	t *testing.T,
	profile *CompactKbuildProfile,
	artifacts []CompactKbuildVisibleArtifact,
) {
	t.Helper()
	artifacts = append([]CompactKbuildVisibleArtifact(nil), artifacts...)
	for index, artifact := range artifacts {
		if artifact.Path == "" || canonicalKbuildRulePath(artifact.Path) != artifact.Path {
			t.Fatalf("test initial-visible artifact %d has noncanonical path %q", index, artifact.Path)
		}
		if index != 0 && artifacts[index-1].Path >= artifact.Path {
			t.Fatalf(
				"test initial-visible artifact paths are not strictly increasing at %q and %q",
				artifacts[index-1].Path, artifact.Path,
			)
		}
	}
	SetCompactKbuildProfileInitialVisibleArtifactView(
		profile,
		staticCompactKbuildInitialVisibleArtifactView{artifacts: artifacts},
	)
}

func mustWriteSource(t *testing.T, root, rel, content string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mustCompactKbuildProfileForTest captures a real evaluator program from Make
// source. Tests use it instead of constructing command-variable snapshots.
func mustCompactKbuildProfileForTest(
	t *testing.T,
	name, makefile, directory, source string,
	variables map[string]string,
) CompactKbuildProfile {
	t.Helper()
	commandLineVariables := map[string]string{}
	for variable, value := range variables {
		refs, err := KbuildActionRoleRefs(value)
		if err != nil {
			t.Fatalf("test Kbuild profile %q variable %s: %v", name, variable, err)
		}
		if len(refs) != 0 {
			commandLineVariables[variable] = value
		}
	}
	kb, err := parseKbuildWithOptions(strings.NewReader(source), makefile, KbuildOptions{
		Variables:               maps.Clone(variables),
		CommandLineVariables:    commandLineVariables,
		ConfigVariablesComplete: true,
		MakeVariablesComplete:   true,
		CaptureTargetEvaluator:  true,
	}, "")
	if err != nil {
		t.Fatalf("parse Kbuild profile %q: %v", name, err)
	}
	profile, err := NewCompactKbuildProfile(name, makefile, "", kb)
	if err != nil {
		t.Fatalf("create Kbuild profile %q: %v", name, err)
	}
	profile.Directory = directory
	return profile
}
