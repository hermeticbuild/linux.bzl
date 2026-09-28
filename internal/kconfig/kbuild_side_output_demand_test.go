package kconfig

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestKbuildSideOutputDemandsUseSourceOrderedRecursivePredecessor(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
EMIT = /selected/emitter
cmd_emit = $(EMIT) -o $@
generated.sym: FORCE
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
CC = /selected/cc
cmd_cc_o_c = $(CC) -c -o $@ $<
drivers/demo.mod.o: drivers/demo.mod.c FORCE
	$(call if_changed,cc_o_c)
.vmlinux.export.o: .vmlinux.export.c FORCE
	$(call if_changed,cc_o_c)
`, nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{
			producer,
			consumer,
		},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "generated.sym", MakeTarget: "generated.sym", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: ".vmlinux.export.o", MakeTarget: ".vmlinux.export.o", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "drivers/demo.mod.o", MakeTarget: "drivers/demo.mod.o", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(demands), 2; got != want {
		t.Fatalf("side-output demands = %#v, want %d", demands, want)
	}
	wantPaths := []string{".vmlinux.export.c", "drivers/demo.mod.c"}
	gotPaths := []string{}
	for _, demand := range demands {
		if got, want := demand.candidates, []compactKbuildSelectionKey{{profile: producer.Name, target: "generated.sym", stage: "target"}}; !slices.Equal(got, want) {
			t.Errorf("demand candidates = %v, want source-ordered candidate %v", got, want)
		}
		if demand.output.Tree != "objects" {
			t.Errorf("demand output = %#v, want consumer's native objects tree", demand.output)
		}
		if demand.stage != "target" || demand.product != "vmlinux" {
			t.Errorf("demand destination = %s/%s, want target/vmlinux", demand.stage, demand.product)
		}
		gotPaths = append(gotPaths, demand.output.Path)
	}
	if !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("demanded paths = %q, want %q", gotPaths, wantPaths)
	}
}

func TestKbuildSideOutputDemandsTraverseLexicalParentPrerequisiteRules(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
stamp: FORCE
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/Makefile.build", "", `
obj := arch/x86/kvm
cmd_ar = touch $@
cmd_cc = touch $@
cmd_gen = touch $@
$(obj)/built-in.a: $(obj)/../../../virt/kvm/kvm_main.o FORCE
	$(call if_changed,ar)
$(obj)/%.o: $(obj)/%.c $(obj)/../../../virt/kvm/kvm_main.generated FORCE
	$(call if_changed,cc)
$(obj)/../../../virt/kvm/kvm_main.generated: opaque.generated.input FORCE
	$(call if_changed,gen)
`, nil)
	consumer = compactKbuildProfileWithSourcesForTest(t, consumer, "virt/kvm/kvm_main.c")
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "stamp", MakeTarget: "stamp", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "arch/x86/kvm/built-in.a", MakeTarget: "arch/x86/kvm/built-in.a", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	// Prime the canonical miss. The recursive lexical lookup must have a
	// distinct cache identity and still select $(obj)/%.o.
	if _, found, err := graph.compactKbuildRuleForProfile(metadata, consumer, "virt/kvm/kvm_main.o"); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("canonical KVM object unexpectedly matched the lexical pattern rule")
	}

	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(demands), 1; got != want || demands[0].output.Path != "opaque.generated.input" {
		t.Fatalf("lexical parent-traversal side-output demands = %#v, want only %q", demands, "opaque.generated.input")
	}
}

func TestKbuildSideOutputDemandsTraverseLexicalSelectedRoot(t *testing.T) {
	const (
		object = "virt/kvm/kvm_main.o"
		alias  = "arch/x86/kvm/../../../virt/kvm/kvm_main.o"
	)
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
stamp: FORCE
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/Makefile.build", "", `
obj := arch/x86/kvm
cmd_cc = touch $@
$(obj)/%.o: $(obj)/%.c opaque.generated.input FORCE
	$(call if_changed,cc)
`, nil)
	consumer = compactKbuildProfileWithSourcesForTest(t, consumer, "virt/kvm/kvm_main.c")
	consumer.Generated = append(consumer.Generated, KbuildTarget{Kind: "targets", Target: object})
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "stamp", MakeTarget: "stamp", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: object, MakeTarget: alias, Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(demands), 1; got != want || demands[0].output.Path != "opaque.generated.input" {
		t.Fatalf("lexical selected-root side-output demands = %#v, want only %q", demands, "opaque.generated.input")
	}
}

func TestKbuildSideOutputCandidateDiscoveryUsesLexicalSelectedRoot(t *testing.T) {
	const (
		producerTarget = "aa/producer.o"
		producerAlias  = "arch/x86/kvm/../../../aa/producer.o"
	)
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/Makefile.build", "", `
obj := arch/x86/kvm
cmd_cc = touch $@
$(obj)/%.o: $(obj)/%.c FORCE
	$(call if_changed,cc)
`, nil)
	producer = compactKbuildProfileWithSourcesForTest(t, producer, "aa/producer.c")
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out: opaque.generated.input FORCE
	$(call if_changed,consume)
`, nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: producerTarget, MakeTarget: producerAlias, Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	wantCandidate := compactKbuildSelectionKey{profile: producer.Name, target: producerTarget, stage: "target"}
	if got, want := len(demands), 1; got != want || !slices.Equal(demands[0].candidates, []compactKbuildSelectionKey{wantCandidate}) {
		t.Fatalf("lexical selected-root side-output candidates = %#v, want %v", demands, wantCandidate)
	}
}

func TestKbuildSideOutputDemandsUseEarlierRecipeInSameInvocation(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "build:demo", "scripts/Makefile.build", "", `
cmd_stamp = touch $@
stamp: FORCE
	$(call if_changed,stamp)
cmd_consume = touch $@
consumer.out: stamp opaque.generated.input FORCE
	$(call if_changed,consume)
`, nil)
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "stamp", MakeTarget: "stamp", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: profile.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(demands), 1; got != want {
		t.Fatalf("side-output demands = %#v, want %d", demands, want)
	}
	want := []compactKbuildSelectionKey{{profile: profile.Name, target: "stamp", stage: "target"}}
	if got := demands[0].candidates; !slices.Equal(got, want) {
		t.Fatalf("same-invocation candidates = %v, want %v", got, want)
	}
}

func TestKbuildNamespacedSourceIsImmutableInputNotSideOutput(t *testing.T) {
	const (
		rustRoot   = "external/rust-src/library"
		rustSource = rustRoot + "/core/src/lib.rs"
	)
	physicalRustRoot := t.TempDir()
	mustWriteSource(t, physicalRustRoot, "core/src/lib.rs", "// core\n")
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
stamp: FORCE
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_rustc = touch $@
rust/core.o: external/rust-src/library/core/src/lib.rs FORCE
	$(call if_changed,rustc)
`, nil)
	consumer.evaluator.template.sourceRoots = map[string]string{rustRoot: physicalRustRoot}
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "stamp", MakeTarget: "stamp", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "rust/core.o", MakeTarget: "rust/core.o", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{
		Config:           config,
		sourceNamespaces: map[string]string{rustRoot: "rust"},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(demands) != 0 {
		t.Fatalf("namespaced Rust source became a side-output demand: %#v", demands)
	}

	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	match, found, err := metadata.compactKbuildRuleForProfile(consumer, "rust/core.o")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("missing evaluated Rust rule")
	}
	consumerKey := compactKbuildSelectionKey{profile: consumer.Name, target: "rust/core.o", stage: "target"}
	builder := newCompactKbuildRulePlanBuilder(metadata, plan).
		withSelectionGraph(graph).
		forSelection(consumerKey, consumer)
	inputs, err := builder.ruleInputs(consumerKey.target, match)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(inputs), 1; got != want || inputs[0].path != rustSource || inputs[0].sourceID == "" || inputs[0].producer != "" {
		t.Fatalf("Rust rule inputs = %#v, want one immutable source edge for %q", inputs, rustSource)
	}
	if got, want := plan.Sources, []ActionPlanSource{{ID: inputs[0].sourceID, Namespace: "rust", Path: rustSource}}; !slices.Equal(got, want) {
		t.Fatalf("plan sources = %#v, want %#v", got, want)
	}
}

func TestKbuildMissingNamespacedPathRemainsSideOutputDemand(t *testing.T) {
	const (
		rustRoot      = "external/rust-src/library"
		missingSource = rustRoot + "/core/src/generated.rs"
	)
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
stamp: FORCE
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_rustc = touch $@
rust/core.o: external/rust-src/library/core/src/generated.rs FORCE
	$(call if_changed,rustc)
`, nil)
	consumer.evaluator.template.sourceRoots = map[string]string{rustRoot: t.TempDir()}
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "stamp", MakeTarget: "stamp", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "rust/core.o", MakeTarget: "rust/core.o", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{
		Config:           config,
		sourceNamespaces: map[string]string{rustRoot: "rust"},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(demands), 1; got != want || demands[0].output.Path != missingSource {
		t.Fatalf("missing namespaced source demands = %#v, want one demand for %q", demands, missingSource)
	}
}

func TestKbuildSideOutputDemandsObserveIntermediatePredecessorRecipes(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
intermediate.out: FORCE
	$(call if_changed,emit)
terminal.out: intermediate.out FORCE
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out: opaque.generated.input FORCE
	$(call if_changed,consume)
`, nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "intermediate.out", MakeTarget: "intermediate.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: producer.Name, Target: "terminal.out", MakeTarget: "terminal.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(demands), 1; got != want {
		t.Fatalf("side-output demands = %#v, want %d", demands, want)
	}
	want := []compactKbuildSelectionKey{
		{profile: producer.Name, target: "intermediate.out", stage: "target"},
		{profile: producer.Name, target: "terminal.out", stage: "target"},
	}
	if got := demands[0].candidates; !slices.Equal(got, want) {
		t.Fatalf("side-output candidates = %v, want complete predecessor recipe closure %v", got, want)
	}
}

func TestKbuildSideOutputDemandsRejectPhonyOnlyPredecessor(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
.PHONY: emit
cmd_emit = touch opaque.generated.input
emit: FORCE
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out: opaque.generated.input FORCE
	$(call if_changed,consume)
`, nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "emit", MakeTarget: "emit", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = graph.compactKbuildSideOutputDemands(&CompactMetadata{Config: config}, config)
	if err == nil || !strings.Contains(err.Error(), "without a source-recipe predecessor action") {
		t.Fatalf("phony side-output candidate error = %v", err)
	}
}

func TestKbuildSideOutputStatesAreSharedAcrossConsumers(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
producer.out: FORCE
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
first.out: opaque.generated.input FORCE
	$(call if_changed,consume)
second.out: opaque.generated.input FORCE
	$(call if_changed,consume)
`, nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "producer.out", MakeTarget: "producer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "first.out", MakeTarget: "first.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "second.out", MakeTarget: "second.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{
		actionRoles:    testConfiguredScopedActionRoles,
		Config:         config,
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	producerNode := compactKbuildOverwriteTestNode(t, plan, graph, producer.Name, "producer.out")
	states := 0
	for _, output := range producerNode.Outputs {
		if output.ObservedPath == "opaque.generated.input" {
			states++
		}
	}
	if got, want := states, 1; got != want {
		t.Fatalf("shared producer state count = %d, want %d: %#v", got, want, producerNode.Outputs)
	}
	resolvers := 0
	physical := map[string]bool{}
	for _, node := range plan.Nodes {
		if node.Tool != "actionfile" || len(node.Outputs) != 1 || node.Outputs[0].Path != "opaque.generated.input" {
			continue
		}
		resolvers++
		physical[actionPlanOutputArtifactPath(node.Outputs[0])] = true
	}
	if resolvers != 2 || len(physical) != 2 {
		t.Fatalf("shared-state resolvers = %d with physical outputs %v, want two consumer-local resolvers", resolvers, physical)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("shared side-output state plan is invalid: %v", err)
	}
}

func TestKbuildGroupedConsumersShareOneSideOutputResolver(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
producer.out: FORCE
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
GEN = `+KbuildActionRoleToken("target", "gen")+`
cmd_group = $(GEN) -o $@ $<
first.out second.out &: opaque.generated.input FORCE
	$(call if_changed,group)
`, nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "producer.out", MakeTarget: "producer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "first.out", MakeTarget: "first.out", GroupedTrigger: "first.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "second.out", MakeTarget: "second.out", GroupedTrigger: "first.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{
		actionRoles:    testScopedActionRoles("gen"),
		Config:         config,
		configFragment: map[string]string{},
	}
	plan := &ActionPlan{
		Toolsets: map[string]string{"target": actionPlanTestProbeIdentity},
		Recipes:  map[string]ActionRecipe{},
	}
	graph, err := metadata.appendGeneratedActionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	first := compactKbuildOverwriteTestNode(t, plan, graph, consumer.Name, "first.out")
	second := compactKbuildOverwriteTestNode(t, plan, graph, consumer.Name, "second.out")
	if first.ID != second.ID {
		t.Fatalf("grouped consumer peers have distinct producers %s and %s", first.ID, second.ID)
	}
	resolvers := []ActionPlanNode{}
	for _, node := range plan.Nodes {
		if node.Tool == "actionfile" && len(node.Outputs) == 1 && node.Outputs[0].Path == "opaque.generated.input" {
			resolvers = append(resolvers, node)
		}
	}
	if got, want := len(resolvers), 1; got != want {
		t.Fatalf("grouped side-output resolver count = %d, want %d: %#v", got, want, resolvers)
	}
	if !compactKbuildOverwriteTestHasProducer(first, resolvers[0].ID) {
		t.Fatalf("grouped consumer inputs = %#v, want shared resolver %s", first.Inputs, resolvers[0].ID)
	}
	if _, err := plan.entries(); err != nil {
		t.Fatalf("grouped side-output resolver plan is invalid: %v", err)
	}
}

func TestKbuildGroupedProducerUnionsPeerPrerequisitesForSideOutputClosure(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
dep-a: FORCE
	$(call if_changed,emit)
dep-b: FORCE
	$(call if_changed,emit)
first.out second.out &: FORCE
	$(call if_changed,emit)
first.out: dep-a
second.out: dep-b
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out: opaque.generated.input FORCE
	$(call if_changed,consume)
`, nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	selection := func(profile, target string) CompactKbuildSelection {
		selection := CompactKbuildSelection{Profile: profile, Target: target, MakeTarget: target, Lifecycle: "target", Scope: "target", Stage: "target"}
		if profile == producer.Name && (target == "first.out" || target == "second.out") {
			selection.GroupedTrigger = "first.out"
		}
		return selection
	}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			selection(producer.Name, "dep-a"),
			selection(producer.Name, "dep-b"),
			selection(producer.Name, "first.out"),
			selection(producer.Name, "second.out"),
			selection(consumer.Name, "consumer.out"),
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.prepareGroupedSelections(metadata, config); err != nil {
		t.Fatal(err)
	}
	key := func(target string) compactKbuildSelectionKey {
		return compactKbuildSelectionKey{profile: producer.Name, target: target, stage: "target"}
	}
	representative := graph.compactKbuildGroupedSelectionRepresentative(key("first.out"))
	if got := graph.compactKbuildGroupedSelectionRepresentative(key("second.out")); got != representative {
		t.Fatalf("grouped peer representative = %s, want %s", compactKbuildSelectionKeyString(got), compactKbuildSelectionKeyString(representative))
	}
	dependencies, err := graph.selectionDependencies(metadata, representative)
	if err != nil {
		t.Fatal(err)
	}
	wantDependencies := []compactKbuildSelectionKey{key("dep-a"), key("dep-b")}
	if !slices.Equal(dependencies, wantDependencies) {
		t.Fatalf("grouped dependency union = %v, want %v", dependencies, wantDependencies)
	}

	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(demands), 1; got != want {
		t.Fatalf("side-output demands = %#v, want %d", demands, want)
	}
	wantCandidates := append(append([]compactKbuildSelectionKey{}, wantDependencies...), representative)
	if got := demands[0].candidates; !slices.Equal(got, wantCandidates) {
		t.Fatalf("grouped predecessor recipe closure = %v, want %v", got, wantCandidates)
	}
	stateGraph, err := graph.compactKbuildSideOutputStateGraph(metadata, demands[0].candidates)
	if err != nil {
		t.Fatal(err)
	}
	for _, dependency := range wantDependencies {
		if got := stateGraph.parents[dependency]; len(got) != 0 {
			t.Fatalf("grouped side-output state parents for %s = %v, want none", compactKbuildSelectionKeyString(dependency), got)
		}
	}
	if got, want := stateGraph.parents[representative], wantDependencies; !slices.Equal(got, want) {
		t.Fatalf("grouped side-output state parents = %v, want dependency frontier %v", got, want)
	}
	if got, want := stateGraph.maximal, []compactKbuildSelectionKey{representative}; !slices.Equal(got, want) {
		t.Fatalf("grouped side-output maximal states = %v, want physical grouped producer %v", got, want)
	}
}

func TestKbuildSideOutputDemandsFollowGeneratedImplicitTarget(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "modpost", "scripts/Makefile.modpost", "", `
cmd_modpost = touch $@
vmlinux.o: FORCE
	$(call if_changed,modpost)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
	consumer := mustCompactKbuildProfileForTest(t, "vmlinux", "scripts/Makefile.vmlinux", "", `
cmd_cc_o_c = touch $@
%.o: %.c FORCE
	$(call if_changed,cc_o_c)
`, nil)
	consumer.Generated = append(consumer.Generated, KbuildTarget{Kind: "targets", Target: ".vmlinux.export.o"})
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "vmlinux.o", MakeTarget: "vmlinux.o", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: ".vmlinux.export.o", MakeTarget: ".vmlinux.export.o", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(demands), 1; got != want || demands[0].output.Path != ".vmlinux.export.c" {
		t.Fatalf("generated implicit side-output demands = %#v, want .vmlinux.export.c", demands)
	}
}

func TestKbuildSideOutputDemandsIgnoreOrderingOnlyDirectoryTarget(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
generated.sym: FORCE
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "resolve-btfids", "tools/bpf/resolve_btfids/Makefile", "tools/bpf/resolve_btfids", `
msg = @printf '  %-8s %s%s\n' "$(1)" "$(notdir $(2))" "$(if $(3), $(3))";
Q = @
cmd_consume = touch $@
consumer.out: libsubcmd FORCE
	$(call if_changed,consume)
libsubcmd:
	$(call msg,MKDIR,,$@)
	$(Q)mkdir -p $(@)
`, nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "generated.sym", MakeTarget: "generated.sym", Lifecycle: "target", Scope: "host", Stage: "host"},
			{Profile: consumer.Name, Target: "tools/bpf/resolve_btfids/consumer.out", MakeTarget: "tools/bpf/resolve_btfids/consumer.out", Lifecycle: "target", Scope: "host", Stage: "host"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(demands) != 0 {
		t.Fatalf("ordering-only directory became side-output demands %#v", demands)
	}
}

func TestKbuildSideOutputDemandsTraverseOrderingOnlyPrerequisiteClosure(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
generated.sym: FORCE
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out: control FORCE
	$(call if_changed,consume)
control: generated.first
control: | generated.second
`, nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "generated.sym", MakeTarget: "generated.sym", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(demands))
	for _, demand := range demands {
		paths = append(paths, demand.output.Path)
	}
	want := []string{"generated.first", "generated.second"}
	if !slices.Equal(paths, want) {
		t.Fatalf("ordering-only prerequisite demands = %q, want %q without control target", paths, want)
	}
}

func TestKbuildSideOutputDemandsRejectStageBoundedFalseOwner(t *testing.T) {
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
bootstrap.out: FORCE
	$(call if_changed,emit)
host.out: bootstrap.out FORCE
	$(call if_changed,emit)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out: missing.side.output FORCE
	$(call if_changed,consume)
`, nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{producer, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producer.Name, Target: "bootstrap.out", MakeTarget: "bootstrap.out", Lifecycle: "target", Scope: "target", Stage: "bootstrap"},
			{Profile: producer.Name, Target: "host.out", MakeTarget: "host.out", Lifecycle: "target", Scope: "host", Stage: "host"},
			{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "bootstrap"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = graph.compactKbuildSideOutputDemands(metadata, config)
	if err == nil || !strings.Contains(err.Error(), "later-stage invocation terminal") {
		t.Fatalf("stage-bounded false-owner error = %v", err)
	}
}

func TestKbuildUnruledPrerequisitesRejectRecursiveImplicitFallback(t *testing.T) {
	profile := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
cmd_shipped = cp $< $@
consumer.out: missing.input FORCE
	$(call if_changed,consume)
%: %_shipped FORCE
	$(call if_changed,shipped)
`, nil)
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{profile},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: profile.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := graph.computeCompactKbuildUnruledPrerequisites(metadata, compactKbuildSelectionKey{
		profile: profile.Name, target: "consumer.out", stage: "target",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := paths, []string{"missing.input"}; !slices.Equal(got, want) {
		t.Fatalf("unruled prerequisites = %q, want %q without recursive _shipped expansion", got, want)
	}
}

func TestKbuildSideOutputDemandsPreserveAmbiguousPredecessorBoundary(t *testing.T) {
	first := mustCompactKbuildProfileForTest(t, "first", "scripts/first.mk", "", `
cmd_first = touch $@
first.out: FORCE
	$(call if_changed,first)
`, nil)
	second := mustCompactKbuildProfileForTest(t, "second", "scripts/second.mk", "", `
cmd_second = touch $@
second.out: FORCE
	$(call if_changed,second)
`, nil)
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out: dynamic.input FORCE
	$(call if_changed,consume)
`, nil)
	consumer.InvocationPredecessors = []string{first.Name, second.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{first, second, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: first.Name, Target: "first.out", MakeTarget: "first.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: second.Name, Target: "second.out", MakeTarget: "second.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	want := []compactKbuildSelectionKey{
		{profile: first.Name, target: "first.out", stage: "target"},
		{profile: second.Name, target: "second.out", stage: "target"},
	}
	if got := demands[0].candidates; !slices.Equal(got, want) {
		t.Fatalf("ambiguous predecessor candidates = %v, want %v", got, want)
	}
}

func TestKbuildSideOutputDemandsDoNotGuessLatestOrderedPredecessorAction(t *testing.T) {
	first := mustCompactKbuildProfileForTest(t, "first", "scripts/first.mk", "", `
cmd_first = touch $@
first.out: FORCE
	$(call if_changed,first)
`, nil)
	second := mustCompactKbuildProfileForTest(t, "second", "scripts/second.mk", "", `
cmd_second = touch $@
second.out: FORCE
	$(call if_changed,second)
`, nil)
	second.InvocationPredecessors = []string{first.Name}
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out: dynamic.input FORCE
	$(call if_changed,consume)
`, nil)
	// The source-order frontier is cumulative: both invocations completed before the
	// consumer, while second's own boundary proves that it completed after first.
	consumer.InvocationPredecessors = []string{first.Name, second.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{first, second, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: first.Name, Target: "first.out", MakeTarget: "first.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: second.Name, Target: "second.out", MakeTarget: "second.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(demands), 1; got != want {
		t.Fatalf("side-output demands = %#v, want %d", demands, want)
	}
	wantCandidates := []compactKbuildSelectionKey{
		{profile: first.Name, target: "first.out", stage: "target"},
		{profile: second.Name, target: "second.out", stage: "target"},
	}
	if got := demands[0].candidates; !slices.Equal(got, wantCandidates) {
		t.Fatalf("side-output candidates = %v, want both ordered actions %v", got, wantCandidates)
	}
	if got, want := demands[0].output.Path, "dynamic.input"; got != want {
		t.Fatalf("side-output path = %q, want %q", got, want)
	}
}

func TestKbuildSideOutputDemandsDoNotGuessLatestVisibleFrontierAction(t *testing.T) {
	first := mustCompactKbuildProfileForTest(t, "first", "scripts/first.mk", "", `
cmd_first = touch $@
first.out: FORCE
	$(call if_changed,first)
`, nil)
	second := mustCompactKbuildProfileForTest(t, "second", "scripts/second.mk", "", `
cmd_second = touch $@
second.out: FORCE
	$(call if_changed,second)
`, nil)
	setTestCompactKbuildInitialVisibleArtifacts(t, &second, []CompactKbuildVisibleArtifact{{
		Path: "first.out", Profile: first.Name, Target: "first.out",
	}})
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
cmd_consume = touch $@
consumer.out: dynamic.input FORCE
	$(call if_changed,consume)
`, nil)
	consumer.InvocationPredecessors = []string{first.Name, second.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{first, second, consumer},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: first.Name, Target: "first.out", MakeTarget: "first.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: second.Name, Target: "second.out", MakeTarget: "second.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumer.Name, Target: "consumer.out", MakeTarget: "consumer.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(demands), 1; got != want {
		t.Fatalf("side-output demands = %#v, want %d", demands, want)
	}
	wantCandidates := []compactKbuildSelectionKey{
		{profile: first.Name, target: "first.out", stage: "target"},
		{profile: second.Name, target: "second.out", stage: "target"},
	}
	if got := demands[0].candidates; !slices.Equal(got, wantCandidates) {
		t.Fatalf("side-output candidates = %v, want both visible-frontier actions %v", got, wantCandidates)
	}
}

func TestKbuildObservedSideOutputsResolveConsumerPrerequisiteSlots(t *testing.T) {
	producerProfile := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
EMIT = /selected/emitter
cmd_emit = $(EMIT) -o $@
generated.sym: FORCE
	$(call if_changed,emit)
`, map[string]string{"EMIT": KbuildActionRoleToken("target", "emit")})
	consumerProfile := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", `
CC = /selected/cc
cmd_cc_o_c = $(CC) -c -o $@ $<
drivers/demo.mod.o: drivers/demo.mod.c FORCE
	$(call if_changed,cc_o_c)
.vmlinux.export.o: .vmlinux.export.c FORCE
	$(call if_changed,cc_o_c)
`, map[string]string{"CC": KbuildActionRoleToken("target", "cc")})
	consumerProfile.InvocationPredecessors = []string{producerProfile.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{
			producerProfile,
			consumerProfile,
		},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: producerProfile.Name, Target: "generated.sym", MakeTarget: "generated.sym", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumerProfile.Name, Target: ".vmlinux.export.o", MakeTarget: ".vmlinux.export.o", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: consumerProfile.Name, Target: "drivers/demo.mod.o", MakeTarget: "drivers/demo.mod.o", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{
		actionRoles: testScopedActionRoles("cc", "emit"),
		Config:      config,
	}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	producerKey := compactKbuildSelectionKey{profile: producerProfile.Name, target: "generated.sym", stage: "target"}
	observations := []compactKbuildObservedOutput{}
	for _, demand := range demands {
		if got, want := demand.candidates, []compactKbuildSelectionKey{producerKey}; !slices.Equal(got, want) {
			t.Fatalf("side-output candidates for %q = %v, want %v", demand.output.Path, got, want)
		}
		observation, err := compactKbuildSideOutputStateObservation(demand, producerKey)
		if err != nil {
			t.Fatal(err)
		}
		observations = append(observations, observation)
	}

	plan := &ActionPlan{Recipes: map[string]ActionRecipe{}}
	plan.selectionGraph = graph
	producerBuilder := newCompactKbuildRulePlanBuilder(metadata, plan).
		withSelectionGraph(graph).
		forSelection(producerKey, producerProfile).
		forOutput("target", "metadata", "module_symvers")
	producerBuilder, err = producerBuilder.forObservedOutputs("generated.sym", observations)
	if err != nil {
		t.Fatal(err)
	}
	producer, err := producerBuilder.build("generated.sym")
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.recordMaterializedProducer(producerKey, producer); err != nil {
		t.Fatal(err)
	}
	producerNode, ok := compactKbuildPlanNode(plan, producer)
	if !ok {
		t.Fatalf("missing observed-output producer %q", producer)
	}
	producerRecipe := plan.Recipes[producerNode.Recipe]

	for _, demand := range demands {
		observation, err := compactKbuildSideOutputStateObservation(demand, producerKey)
		if err != nil {
			t.Fatal(err)
		}
		stateSlot := -1
		for slot, output := range producerNode.Outputs {
			if output.Tree == observation.output.Tree && output.Path == observation.output.Path {
				stateSlot = slot
				if got := output.ObservedPath; got != demand.output.Path {
					t.Fatalf("state %q observed path = %q, want %q", output.Path, got, demand.output.Path)
				}
			}
		}
		if stateSlot < 0 {
			t.Fatalf("producer outputs = %#v, want state %#v", producerNode.Outputs, observation.output)
		}
		binding := planOrdinal(stateSlot)
		if got := producerRecipe.ObservedOutputs[binding]; got != demand.output.Path {
			t.Fatalf("producer observed output %s = %q, want %q", binding, got, demand.output.Path)
		}
		if got := producerRecipe.WorkingOutputs[binding]; got != "" {
			t.Fatalf("producer state %s is also a command-owned working output %q", binding, got)
		}
		if got := producerRecipe.ObservedOutputBases[binding]; len(got) != 0 {
			t.Fatalf("root producer state %s has predecessor bases %q, want none", binding, got)
		}

		resolved, err := appendCompactKbuildSideOutputResolver(
			plan, graph, metadata, demand,
			[]compactKbuildSideOutputCandidateState{{
				candidate: producerKey,
				input: compactKbuildRuleInput{
					path: observation.output.Path, producer: producer, slot: stateSlot,
				},
			}},
		)
		if err != nil {
			t.Fatal(err)
		}
		resolverNode, ok := compactKbuildPlanNode(plan, resolved.producer)
		if !ok {
			t.Fatalf("missing side-output resolver %q", resolved.producer)
		}
		if got, want := resolverNode.Inputs, []ActionPlanNodeEdge{{
			Role: "state", ProducerID: producer, Slot: stateSlot,
		}}; !slices.Equal(got, want) {
			t.Fatalf("resolver inputs = %#v, want %#v", got, want)
		}
		resolverRecipe := plan.Recipes[resolverNode.Recipe]
		if got, want := resolverRecipe.Inputs, []string{"state:00000000"}; !slices.Equal(got, want) {
			t.Fatalf("resolver input bindings = %q, want %q", got, want)
		}
		if got, want := resolverRecipe.Arguments, []string{
			"-out", "${output:00000000}", "-state", "${input:state:00000000}",
		}; !slices.Equal(got, want) {
			t.Fatalf("resolver arguments = %q, want %q", got, want)
		}
		if !resolverRecipe.ArgumentsFile {
			t.Fatalf("resolver recipe does not use canonical arguments-file protocol: %#v", resolverRecipe)
		}

		consumerKey := demand.consumer
		builder := newCompactKbuildRulePlanBuilder(metadata, plan).
			withSelectionGraph(graph).
			forSelection(consumerKey, consumerProfile).
			forOutput("target", "objects", "vmlinux").
			withResolvedSideOutputs(map[string]compactKbuildRuleInput{demand.output.Path: resolved})
		consumer, err := builder.build(consumerKey.target)
		if err != nil {
			t.Fatal(err)
		}
		consumerNode, ok := compactKbuildPlanNode(plan, consumer)
		if !ok {
			t.Fatalf("missing side-output consumer %q", consumer)
		}
		bound := false
		for _, input := range consumerNode.Inputs {
			if input.ProducerID == resolved.producer && input.Slot == resolved.slot {
				bound = true
			}
		}
		if !bound {
			t.Fatalf("consumer %q inputs = %#v, want resolver %s slot %d", consumerKey.target, consumerNode.Inputs, resolved.producer, resolved.slot)
		}
	}
}

func TestKbuildInvocationPredecessorCycleIsRejected(t *testing.T) {
	first := mustCompactKbuildProfileForTest(t, "first", "scripts/first.mk", "", `
cmd_first = touch $@
first.out: FORCE
	$(call if_changed,first)
`, nil)
	second := mustCompactKbuildProfileForTest(t, "second", "scripts/second.mk", "", `
cmd_second = touch $@
second.out: FORCE
	$(call if_changed,second)
`, nil)
	first.InvocationPredecessors = []string{second.Name}
	second.InvocationPredecessors = []string{first.Name}
	config := CompactConfig{
		KbuildProfiles: []CompactKbuildProfile{first, second},
		KbuildSelections: []CompactKbuildSelection{
			{Profile: first.Name, Target: "first.out", MakeTarget: "first.out", Lifecycle: "target", Scope: "target", Stage: "target"},
			{Profile: second.Name, Target: "second.out", MakeTarget: "second.out", Lifecycle: "target", Scope: "target", Stage: "target"},
		},
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = graph.materializationOrder(metadata)
	if err == nil || !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("invocation predecessor cycle error = %v", err)
	}
}

func TestKbuildSelectionPlanningCachesSharedPredecessorAndRuleWork(t *testing.T) {
	const consumerCount = 128
	producer := mustCompactKbuildProfileForTest(t, "producer", "scripts/producer.mk", "", `
cmd_emit = touch $@
generated.sym: FORCE
	$(call if_changed,emit)
`, nil)
	var source strings.Builder
	source.WriteString("cmd_consume = touch $@\n")
	selections := []CompactKbuildSelection{{Profile: producer.Name, Target: "generated.sym", MakeTarget: "generated.sym", Lifecycle: "target", Scope: "target", Stage: "target"}}
	for index := range consumerCount {
		target := fmt.Sprintf("consumer/%03d.out", index)
		prerequisite := fmt.Sprintf("generated/%03d.input", index)
		fmt.Fprintf(&source, "%s: %s FORCE\n\t$(call if_changed,consume)\n", target, prerequisite)
		selections = append(selections, CompactKbuildSelection{
			Profile: "consumer", Target: target, MakeTarget: target, Lifecycle: "target", Scope: "target", Stage: "target",
		})
	}
	consumer := mustCompactKbuildProfileForTest(t, "consumer", "scripts/consumer.mk", "", source.String(), nil)
	consumer.InvocationPredecessors = []string{producer.Name}
	config := CompactConfig{
		KbuildProfiles:   []CompactKbuildProfile{producer, consumer},
		KbuildSelections: selections,
	}
	metadata := &CompactMetadata{Config: config}
	graph, err := newCompactKbuildSelectionGraph(config)
	if err != nil {
		t.Fatal(err)
	}

	demands, err := graph.compactKbuildSideOutputDemands(metadata, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(demands), consumerCount; got != want {
		t.Fatalf("side-output demand count = %d, want %d", got, want)
	}
	if _, err := graph.materializationOrder(metadata); err != nil {
		t.Fatal(err)
	}
	wantMisses := compactKbuildSelectionGraphCacheMisses{
		ruleResolution:        2*consumerCount + 1,
		ruleEvaluation:        2*consumerCount + 1,
		targetRuleContext:     consumerCount + 1,
		sourceEvidence:        consumerCount,
		nativeDependencies:    consumerCount + 1,
		invocationPredecessor: 1,
		terminalSelection:     1,
		// Discovery checks every selected recipe, including the producer, so an
		// opaque prerequisite without any predecessor cannot be silently ignored.
		unruledPrerequisite: consumerCount + 1,
	}
	if got := graph.cacheMisses; got != wantMisses {
		t.Fatalf("selection-planning cache misses = %#v, want one per unique lookup %#v", got, wantMisses)
	}

	// Side-output discovery and materialization share the same immutable graph.
	// A second pass must not repeat any exact-rule viability scans, recursive
	// prerequisite walks, or terminal-predecessor searches.
	if _, err := graph.compactKbuildSideOutputDemands(metadata, config); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.materializationOrder(metadata); err != nil {
		t.Fatal(err)
	}
	if got := graph.cacheMisses; got != wantMisses {
		t.Fatalf("repeated selection planning added cache misses: got %#v, want %#v", got, wantMisses)
	}
	if got, want := len(graph.terminalSelections), 1; got != want {
		t.Fatalf("terminal predecessor cache size = %d, want one profile/stage entry", got)
	}
}
