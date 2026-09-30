package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func compactKbuildSourcePhaseGraphFixture(t *testing.T) CompactConfig {
	t.Helper()
	root := t.TempDir()
	source := compactKbuildLinkVmlinuxFixture(true)
	mustWriteSource(t, root, "scripts/link-vmlinux.sh", source)
	mustWriteSource(t, root, "scripts/mksysmap", "#!/bin/sh\n$NM -n $1 | grep -v ' U ' > $2\n")
	analyzed, selected, err := AnalyzeCompactKbuildLinkVmlinuxPhases(source)
	if err != nil || !selected {
		t.Fatalf("analyze fixture source: selected=%t, error=%v", selected, err)
	}
	parent := mustCompactKbuildProfileForTest(t, "link-parent", "Makefile", "", `
vmlinux: scripts/link-vmlinux.sh
	sh scripts/link-vmlinux.sh ld -z defs
`, nil)
	parent.evaluator.template.sourceRoots = map[string]string{"__LINUX_BZL_SOURCE_TREE__": root}
	parent.SelectedSourceScriptPhases = []CompactKbuildSelectedSourcePhase{
		{OwnerTarget: "vmlinux", OutputPath: ".version", SourcePath: "scripts/link-vmlinux.sh",
			Ordinal: 0, SourceSHA256: analyzed.SourceSHA256, Spans: slices.Clone(analyzed.VersionSpans),
			SourceArguments: []string{"ld", "-z", "defs"}},
		{OwnerTarget: "vmlinux", OutputPath: "vmlinux.o", SourcePath: "scripts/link-vmlinux.sh",
			Ordinal: 1, SourceSHA256: analyzed.SourceSHA256, Spans: slices.Clone(analyzed.ObjectSpans),
			SourceArguments: []string{"ld", "-z", "defs"}},
	}
	first := mustCompactKbuildProfileForTest(t, "init-child", "scripts/Makefile.build", "", `
init/built-in.a: FORCE
	touch $@
`, nil)
	second := mustCompactKbuildProfileForTest(t, "modpost-child", "scripts/Makefile.modpost", "", `
vmlinux.symvers: vmlinux.o FORCE
	touch $@
`, nil)
	parent.TargetInvocationDependencies = []CompactKbuildInvocationDependency{
		{Target: "vmlinux", Profile: first.Name, Goals: []string{"init/built-in.a"}, SourcePhaseBefore: ".version"},
		{Target: "vmlinux", Profile: second.Name, Goals: []string{"vmlinux.symvers"}, SourcePhaseBefore: "vmlinux.o"},
	}
	second.InvocationPredecessors = []string{first.Name}
	return CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{parent, first, second},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: parent.Name, Target: "vmlinux", MakeTarget: "vmlinux", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: parent.Name, Target: ".version", MakeTarget: ".version", SourceScriptPhase: "version", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: parent.Name, Target: "vmlinux.o", MakeTarget: "vmlinux.o", SourceScriptPhase: "object", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: first.Name, Target: "init/built-in.a", MakeTarget: "init/built-in.a", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: second.Name, Target: "vmlinux.symvers", MakeTarget: "vmlinux.symvers", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
}

func TestSelectedSourcePhaseGraphOrdersVersionInitObjectModpostFinal(t *testing.T) {
	config := compactKbuildSourcePhaseGraphFixture(t)
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	metadata := &CompactMetadata{Config: config}
	if err := graph.prepareGroupedSelections(metadata, config); err != nil {
		t.Fatal(err)
	}
	ordered, err := graph.materializationOrder(metadata)
	if err != nil {
		t.Fatal(err)
	}
	actual := make([]string, 0, len(ordered))
	for _, selection := range ordered {
		actual = append(actual, selection.Target)
	}
	want := []string{".version", "init/built-in.a", "vmlinux.o", "vmlinux.symvers", "vmlinux"}
	if !slices.Equal(actual, want) {
		t.Fatalf("source selected materialization order = %q, want %q", actual, want)
	}
	versionKey := compactKbuildSelectionKey{profile: config.KbuildProfiles[0].Name, target: ".version", stage: "target"}
	phase, owner, found := graph.selectedSourcePhase(versionKey)
	if !found || phase.Ordinal != 0 || phase.SourceSHA256 == "" || owner.target != "vmlinux" {
		t.Fatalf("source phase provenance = %+v, owner %+v, found %t", phase, owner, found)
	}
	rootKey := compactKbuildSelectionKey{profile: owner.profile, target: "vmlinux", stage: "target"}
	finalDependencies, err := graph.selectionDependencies(metadata, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalDependencies) != 1 || finalDependencies[0].target != "vmlinux.symvers" {
		t.Fatalf("final vmlinux dependencies = %+v, want only modpost child", finalDependencies)
	}
}

func TestSelectedSourcePhaseStartsAfterInvokingGoalPrerequisite(t *testing.T) {
	for _, test := range []struct {
		name, goal string
		wantGate   bool
	}{
		{name: "source goal prerequisite", goal: "all: gate", wantGate: true},
		{name: "visible but unrelated output", goal: "all:", wantGate: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := compactKbuildSourcePhaseGraphFixture(t)
			parent := &config.KbuildProfiles[0]
			outer := mustCompactKbuildProfileForTest(t, "outer-invocation", "outer/Makefile", "", `
.PHONY: all
`+test.goal+`
	$(MAKE) -f Makefile vmlinux
gate:
	touch $@
`, nil)
			outer.TargetInvocationDependencies = []CompactKbuildInvocationDependency{
				{Target: "all", Profile: parent.Name, Goals: []string{"vmlinux"}},
			}
			setTestCompactKbuildInitialVisibleArtifacts(t, parent, []CompactKbuildVisibleArtifact{
				{Path: "gate", Profile: outer.Name, Target: "gate"},
			})
			config.KbuildProfiles = append(config.KbuildProfiles, outer)
			config.KbuildSelections = append(config.KbuildSelections, CompactKbuildSelection{
				Profile: outer.Name, Target: "gate", MakeTarget: "gate",
				Lifecycle: "target", Scope: "target", Stage: "target",
			})
			graph, err := newCompactKbuildSelectionGraph(config)
			if err != nil {
				t.Fatal(err)
			}
			metadata := &CompactMetadata{Config: config}
			if err := graph.prepareGroupedSelections(metadata, config); err != nil {
				t.Fatal(err)
			}
			phase := compactKbuildSelectionKey{profile: parent.Name, target: ".version", stage: "target"}
			dependencies, err := graph.selectionDependencies(metadata, phase)
			if err != nil {
				t.Fatal(err)
			}
			gate := compactKbuildSelectionKey{profile: outer.Name, target: "gate", stage: "target"}
			if slices.Contains(dependencies, gate) != test.wantGate {
				t.Fatalf("source phase owner prerequisites = %#v, gate selected = %t", dependencies, test.wantGate)
			}
			for _, future := range []string{"init/built-in.a", "vmlinux.o", "vmlinux.symvers"} {
				if slices.ContainsFunc(dependencies, func(key compactKbuildSelectionKey) bool { return key.target == future }) {
					t.Errorf("source phase imported later child %q in %#v", future, dependencies)
				}
			}
		})
	}
}

func TestSelectedSourcePhaseGraphRejectsUnboundProvenance(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*CompactConfig)
		wantErr string
	}{
		{"missing selected phase", func(config *CompactConfig) {
			config.KbuildSelections = append(config.KbuildSelections[:1], config.KbuildSelections[2:]...)
		}, "no exact phase selection"},
		{"wrong script digest", func(config *CompactConfig) {
			config.KbuildProfiles[0].SelectedSourceScriptPhases[0].SourceSHA256 = strings.Repeat("0", 64)
		}, "does not match exact source write"},
		{"wrong source spans", func(config *CompactConfig) {
			config.KbuildProfiles[0].SelectedSourceScriptPhases[1].Spans = []CompactKbuildLinkVmlinuxSourceSpan{{Start: 0, End: 1}}
		}, "does not match exact source write"},
		{"missing source arguments", func(config *CompactConfig) {
			config.KbuildProfiles[0].SelectedSourceScriptPhases[0].SourceArguments = nil
		}, "missing or inconsistent source arguments"},
		{"inconsistent source arguments", func(config *CompactConfig) {
			config.KbuildProfiles[0].SelectedSourceScriptPhases[0].SourceArguments[0] = "cc"
		}, "missing or inconsistent source arguments"},
		{"forged source arguments", func(config *CompactConfig) {
			for index := range config.KbuildProfiles[0].SelectedSourceScriptPhases {
				config.KbuildProfiles[0].SelectedSourceScriptPhases[index].SourceArguments = []string{"cc", "-z", "defs"}
			}
		}, "source arguments differ from the exact selected recipe"},
		{"script not invoked by selected recipe", func(config *CompactConfig) {
			config.KbuildProfiles[0].Rules[0].Recipe[0] = "sh scripts/other-script.sh ld -z defs"
		}, "no exact selected recipe invocation"},
		{"missing parent source prerequisite", func(config *CompactConfig) {
			config.KbuildProfiles[0].Rules[0].Prerequisites = nil
		}, "exact Make rules declaring"},
		{"missing child boundary", func(config *CompactConfig) {
			config.KbuildProfiles[0].TargetInvocationDependencies[0].SourcePhaseBefore = ""
		}, "unbound or repeated child boundary"},
		{"wrong child makefile", func(config *CompactConfig) {
			config.KbuildProfiles[2].Path = "scripts/Makefile.build"
		}, "does not match source Make boundary"},
		{"unsupported phase kind", func(config *CompactConfig) {
			config.KbuildSelections[1].SourceScriptPhase = "final"
		}, "no exact source script phase"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := compactKbuildSourcePhaseGraphFixture(t)
			test.mutate(&config)
			if _, err := newCompactKbuildSelectionGraph(config); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("invalid source phase graph error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
