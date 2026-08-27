package kconfig

import (
	"slices"
	"strings"
	"testing"
)

type fakeCompactKbuildInitialVisibleArtifactView struct {
	artifacts    []CompactKbuildVisibleArtifact
	lengthOffset int
	get          func(string) (CompactKbuildVisibleArtifact, bool)
	getCalls     int
	rangeCalls   int
}

func (v *fakeCompactKbuildInitialVisibleArtifactView) Len() int {
	return len(v.artifacts) + v.lengthOffset
}

func (v *fakeCompactKbuildInitialVisibleArtifactView) Get(path string) (CompactKbuildVisibleArtifact, bool) {
	v.getCalls++
	if v.get != nil {
		return v.get(path)
	}
	for _, artifact := range v.artifacts {
		if artifact.Path == path {
			return artifact, true
		}
	}
	return CompactKbuildVisibleArtifact{}, false
}

func (v *fakeCompactKbuildInitialVisibleArtifactView) Range(
	prefix string,
	visit func(CompactKbuildVisibleArtifact) bool,
) {
	v.rangeCalls++
	for _, artifact := range v.artifacts {
		if prefix != "" && !strings.HasPrefix(artifact.Path, prefix) {
			continue
		}
		if !visit(artifact) {
			return
		}
	}
}

func TestCompactKbuildProfileInitialVisibleArtifactViewAccessors(t *testing.T) {
	artifacts := []CompactKbuildVisibleArtifact{
		{Path: "dir/first.h", Profile: "owner", Target: "first"},
		{Path: "dir/nested/second.h", Profile: "owner", Target: "second"},
		{Path: "other.h", Profile: "owner", Target: "other"},
	}
	profile := CompactKbuildProfile{}
	view := &fakeCompactKbuildInitialVisibleArtifactView{artifacts: artifacts}
	SetCompactKbuildProfileInitialVisibleArtifactView(&profile, view)

	if got, want := CompactKbuildProfileInitialVisibleArtifactCount(profile), len(artifacts); got != want {
		t.Fatalf("frontier length = %d, want %d", got, want)
	}
	for _, want := range artifacts {
		got, ok := CompactKbuildProfileInitialVisibleArtifact(profile, want.Path)
		if !ok || got != want {
			t.Fatalf("frontier[%q] = (%#v, %t), want (%#v, true)", want.Path, got, ok, want)
		}
	}
	if got, ok := CompactKbuildProfileInitialVisibleArtifact(profile, "missing"); ok {
		t.Fatalf("missing frontier lookup = %#v, want absent", got)
	}

	all := []CompactKbuildVisibleArtifact{}
	RangeCompactKbuildProfileInitialVisibleArtifacts(profile, "", func(artifact CompactKbuildVisibleArtifact) bool {
		all = append(all, artifact)
		return true
	})
	if !slices.Equal(all, artifacts) {
		t.Fatalf("complete frontier range = %#v, want %#v", all, artifacts)
	}
	prefix := []CompactKbuildVisibleArtifact{}
	RangeCompactKbuildProfileInitialVisibleArtifacts(profile, "dir/", func(artifact CompactKbuildVisibleArtifact) bool {
		prefix = append(prefix, artifact)
		return true
	})
	if !slices.Equal(prefix, artifacts[:2]) {
		t.Fatalf("frontier prefix range = %#v, want %#v", prefix, artifacts[:2])
	}
}

func TestValidateCompactKbuildProfilesAcceptsInitialVisibleArtifactView(t *testing.T) {
	artifact := CompactKbuildVisibleArtifact{
		Path: "generated/header.h", Profile: "owner", Target: "generated/header.h",
	}
	profiles := []CompactKbuildProfile{
		{Name: "owner", Path: "scripts/owner.mk"},
		{Name: "consumer", Path: "scripts/consumer.mk"},
	}
	view := &fakeCompactKbuildInitialVisibleArtifactView{artifacts: []CompactKbuildVisibleArtifact{artifact}}
	SetCompactKbuildProfileInitialVisibleArtifactView(
		&profiles[1],
		view,
	)
	if err := validateCompactKbuildProfiles(profiles); err != nil {
		t.Fatalf("validateCompactKbuildProfiles() error = %v", err)
	}
	if view.rangeCalls != 0 || view.getCalls != 0 {
		t.Fatalf("profile validation enumerated canonical view: range calls %d, get calls %d", view.rangeCalls, view.getCalls)
	}
	graph, err := newCompactKbuildSelectionGraph(CompactConfig{KbuildProfiles: profiles})
	if err != nil {
		t.Fatalf("newCompactKbuildSelectionGraph() error = %v", err)
	}
	if view.rangeCalls != 0 || view.getCalls != 0 {
		t.Fatalf("selection graph construction enumerated canonical view: range calls %d, get calls %d", view.rangeCalls, view.getCalls)
	}
	got, ok := graph.compactKbuildInitialVisibleArtifact("consumer", artifact.Path)
	if !ok || got != artifact {
		t.Fatalf("graph frontier lookup = (%#v, %t), want (%#v, true)", got, ok, artifact)
	}
	if view.rangeCalls != 0 || view.getCalls != 1 {
		t.Fatalf("exact graph lookup calls = (range %d, get %d), want (0, 1)", view.rangeCalls, view.getCalls)
	}
}

func TestCompactKbuildSelectionGraphValidatesSelectedViewArtifactThroughGet(t *testing.T) {
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
consumer.out:
	touch $@
`, nil)
	owner := mustCompactKbuildProfileForTest(t, "owner", "scripts/owner.mk", "", `
generated/header.h:
	touch $@
`, nil)
	artifact := CompactKbuildVisibleArtifact{
		Path: "generated/header.h", Profile: owner.Name, Target: "generated/header.h",
	}
	view := &fakeCompactKbuildInitialVisibleArtifactView{
		artifacts: []CompactKbuildVisibleArtifact{artifact},
		get: func(string) (CompactKbuildVisibleArtifact, bool) {
			return CompactKbuildVisibleArtifact{}, false
		},
	}
	SetCompactKbuildProfileInitialVisibleArtifactView(&consumer, view)
	_, err := newCompactKbuildSelectionGraph(CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{consumer, owner},
		KbuildSelections: []CompactKbuildSelection{
			{
				Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target",
				UsesInitialObjectTree: true,
				InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts(
					[]CompactKbuildVisibleArtifact{artifact},
				),
			},
			{Profile: owner.Name, Target: artifact.Target, MakeTarget: artifact.Target, Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "outside its exact visible frontier") {
		t.Fatalf("selected view artifact validation error = %v", err)
	}
	if view.rangeCalls != 0 || view.getCalls != 1 {
		t.Fatalf("selected artifact validation calls = (range %d, get %d), want (0, 1)", view.rangeCalls, view.getCalls)
	}
}

func TestCompactKbuildSelectionGraphRejectsInvalidInitialVisibleArtifactPath(t *testing.T) {
	for _, artifactPath := range []string{
		"./generated/header.h",
		"generated//header.h",
		"../generated/header.h",
		"generated/",
		"generated/%.h",
		"generated/$header.h",
	} {
		t.Run(strings.NewReplacer("/", "_", "$", "_").Replace(artifactPath), func(t *testing.T) {
			artifact := CompactKbuildVisibleArtifact{
				Path: artifactPath, Profile: "owner", Target: "generated/header.h",
			}
			profiles := []CompactKbuildProfile{
				{Name: "owner", Path: "scripts/owner.mk"},
				{Name: "consumer", Path: "scripts/consumer.mk"},
			}
			SetCompactKbuildProfileInitialVisibleArtifactView(
				&profiles[1],
				&fakeCompactKbuildInitialVisibleArtifactView{artifacts: []CompactKbuildVisibleArtifact{artifact}},
			)
			_, err := newCompactKbuildSelectionGraph(CompactConfig{
				KbuildProfiles: profiles,
				KbuildSelections: []CompactKbuildSelection{{
					Profile: "consumer", Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target",
					UsesInitialObjectTree: true,
					InitialObjectTreeArtifacts: EncodeCompactKbuildInitialObjectTreeArtifacts(
						[]CompactKbuildVisibleArtifact{artifact},
					),
				}},
			})
			if err == nil {
				t.Fatalf("newCompactKbuildSelectionGraph() error = %v for path %q", err, artifactPath)
			}
		})
	}
}
