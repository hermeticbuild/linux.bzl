package kconfig

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func requireKbuildSymbolicStatePlanStable(t *testing.T, discovery, replay *ProbePlan) {
	t.Helper()
	if !slices.Equal(discovery.Terminal, replay.Terminal) || len(discovery.Nodes) != len(replay.Nodes) {
		t.Fatalf("replay plan = %#v, want discovery %#v", replay, discovery)
	}
	for index := range discovery.Nodes {
		got, want := replay.Nodes[index], discovery.Nodes[index]
		if got.ID != want.ID || got.RequestID != want.RequestID || got.Scope != want.Scope ||
			!slices.Equal(got.Inputs, want.Inputs) {
			t.Fatalf("replay node %d = %#v, want %#v", index, got, want)
		}
	}
}

func requireKbuildSymbolicStatePlanHasNoRawAtoms(t *testing.T, plan *ProbePlan) {
	t.Helper()
	encoded, err := json.Marshal(plan.Requests)
	if err != nil {
		t.Fatal(err)
	}
	if atom := linuxProbeSymbolPattern.Find(encoded); atom != nil {
		t.Fatalf("raw symbolic atom %q reached a probe request: %s", atom, encoded)
	}
}

func replayKbuildSymbolicStateEvaluation[T any](
	t *testing.T,
	discovery *KbuildProbeEvaluation[T],
	opts KbuildProbeWorkloadOptions,
	workload func(*KbuildProbeScopes) (T, error),
	supported bool,
) *KbuildProbeEvaluation[T] {
	t.Helper()
	roots := writeKbuildProbeResults(t, discovery.Plan, map[string]bool{"target": supported})
	oracle, err := NewProbeResultOracleFromTrees(
		map[string]string{"target": roots["target"]},
		discovery.Plan.Toolsets,
	)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := EvaluateKbuildProbeWorkload(opts, oracle, workload)
	if err != nil {
		t.Fatal(err)
	}
	requireKbuildSymbolicStatePlanStable(t, discovery.Plan, replay.Plan)
	requireKbuildSymbolicStatePlanHasNoRawAtoms(t, replay.Plan)
	return replay
}

func TestKbuildCorrelatedOrderedElseIfNeverActivatesImpossibleBranch(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct {
		Selected   string
		Downstream string
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"selected", "downstream"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(symbolicHardeningCompilerFixture+`
ifneq ($(first),)
selected := -fbranch-a
else ifneq ($(first),)
selected := -fbranch-b
else
selected := -fbranch-c
endif
downstream := $(call cc-option,$(selected))
`), "symbolic/correlated.mk", options, "")
		if err != nil {
			return result{}, err
		}
		return result{Selected: parsed.Variables["selected"], Downstream: parsed.Variables["downstream"]}, nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(discovery.Value.Selected, "branch-b") {
		t.Fatalf("impossible correlated branch leaked into discovery value %q", discovery.Value.Selected)
	}
	if len(discovery.Plan.Nodes) != 2 {
		t.Fatalf("correlated branch plan nodes = %#v, want source and downstream probes", discovery.Plan.Nodes)
	}
	requireKbuildSymbolicStatePlanHasNoRawAtoms(t, discovery.Plan)

	for _, test := range []struct {
		name, selected string
		supported      bool
	}{
		{name: "first true", supported: true, selected: "-fbranch-a"},
		{name: "first false", supported: false, selected: "-fbranch-c"},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay := replayKbuildSymbolicStateEvaluation(t, discovery, opts, workload, test.supported)
			if replay.Value.Selected != test.selected {
				t.Fatalf("selected = %q, want %q", replay.Value.Selected, test.selected)
			}
			if strings.Contains(replay.Value.Selected, "branch-b") {
				t.Fatalf("impossible correlated branch selected during replay: %#v", replay.Value)
			}
		})
	}
}

func TestKbuildGuardedAppendPreservesKnownVariableIdentity(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct {
		Simple, Recursive string
		SimpleDefined     string
		RecursiveDefined  string
		SimpleOrigin      string
		SimpleFlavor      string
		RecursiveOrigin   string
		RecursiveFlavor   string
		Downstream        string
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables: []string{
				"simple", "recursive", "simple_defined", "recursive_defined", "simple_origin", "simple_flavor",
				"recursive_origin", "recursive_flavor", "downstream",
			},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(symbolicHardeningCompilerFixture+`
simple := -fbase-simple
recursive = -fbase-recursive
ifneq ($(first),)
simple += -fselected-simple
recursive += -fselected-recursive
endif
ifdef simple
simple_defined := yes
endif
ifdef recursive
recursive_defined := yes
endif
simple_origin := $(origin simple)
simple_flavor := $(flavor simple)
recursive_origin := $(origin recursive)
recursive_flavor := $(flavor recursive)
downstream := $(call cc-option,$(simple) $(recursive))
`), "symbolic/known-append.mk", options, "")
		if err != nil {
			return result{}, err
		}
		return result{
			Simple: parsed.Variables["simple"], Recursive: parsed.Variables["recursive"],
			SimpleDefined: parsed.Variables["simple_defined"], RecursiveDefined: parsed.Variables["recursive_defined"],
			SimpleOrigin: parsed.Variables["simple_origin"], SimpleFlavor: parsed.Variables["simple_flavor"],
			RecursiveOrigin: parsed.Variables["recursive_origin"], RecursiveFlavor: parsed.Variables["recursive_flavor"],
			Downstream: parsed.Variables["downstream"],
		}, nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.SimpleOrigin != "file" || discovery.Value.SimpleFlavor != "simple" ||
		discovery.Value.RecursiveOrigin != "file" || discovery.Value.RecursiveFlavor != "recursive" ||
		discovery.Value.SimpleDefined != "yes" || discovery.Value.RecursiveDefined != "yes" {
		t.Fatalf("guarded append identity = %#v", discovery.Value)
	}
	requireKbuildSymbolicStatePlanHasNoRawAtoms(t, discovery.Plan)

	for _, test := range []struct {
		name, simple, recursive string
		supported               bool
	}{
		{name: "selected", supported: true, simple: "-fbase-simple -fselected-simple", recursive: "-fbase-recursive -fselected-recursive"},
		{name: "not selected", supported: false, simple: "-fbase-simple", recursive: "-fbase-recursive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay := replayKbuildSymbolicStateEvaluation(t, discovery, opts, workload, test.supported)
			if strings.TrimSpace(replay.Value.Simple) != test.simple || strings.TrimSpace(replay.Value.Recursive) != test.recursive {
				t.Fatalf("guarded append replay = %#v, want simple %q recursive %q", replay.Value, test.simple, test.recursive)
			}
		})
	}
}

func TestKbuildComplementaryRecursiveAssignmentsRetainLinuxLP64Selection(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct {
		Relative, Libdir, PackageConfig string
		Defined, Origin, Flavor         string
		Downstream                      string
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables: []string{
				"libdir_relative", "libdir", "pkgconfig", "relative_defined",
				"relative_origin", "relative_flavor", "downstream",
			},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(symbolicHardeningCompilerFixture+`
LP64 := $(if $(first),1,0)
ifeq ($(LP64),1)
libdir_relative = lib64
else
libdir_relative = lib
endif
prefix = /usr/local
libdir = $(prefix)/$(libdir_relative)
pkgconfig = $(libdir)/pkgconfig
ifdef libdir_relative
relative_defined := yes
endif
relative_origin := $(origin libdir_relative)
relative_flavor := $(flavor libdir_relative)
downstream = $(libdir_relative):$(pkgconfig)
`), "tools/lib/bpf/Makefile", options, "")
		if err != nil {
			return result{}, err
		}
		return result{
			Relative: parsed.Variables["libdir_relative"], Libdir: parsed.Variables["libdir"],
			PackageConfig: parsed.Variables["pkgconfig"], Defined: parsed.Variables["relative_defined"],
			Origin: parsed.Variables["relative_origin"], Flavor: parsed.Variables["relative_flavor"],
			Downstream: parsed.Variables["downstream"],
		}, nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.Defined != "yes" || discovery.Value.Origin != "file" || discovery.Value.Flavor != "recursive" {
		t.Fatalf("LP64 recursive assignment identity = %#v, want defined file/recursive", discovery.Value)
	}
	for name, value := range map[string]string{
		"relative": discovery.Value.Relative, "libdir": discovery.Value.Libdir,
		"pkgconfig": discovery.Value.PackageConfig, "downstream": discovery.Value.Downstream,
	} {
		if !linuxProbeSymbolPattern.MatchString(value) {
			t.Fatalf("discovery %s = %q, want retained LP64 selection", name, value)
		}
	}
	if len(discovery.Plan.Nodes) != 1 {
		t.Fatalf("LP64 discovery graph = %#v, want one source probe with downstream Make use retained symbolically", discovery.Plan.Nodes)
	}
	requireKbuildSymbolicStatePlanHasNoRawAtoms(t, discovery.Plan)

	for _, test := range []struct {
		name, relative, libdir string
		supported              bool
	}{
		{name: "LP64", supported: true, relative: "lib64", libdir: "/usr/local/lib64"},
		{name: "non-LP64", supported: false, relative: "lib", libdir: "/usr/local/lib"},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay := replayKbuildSymbolicStateEvaluation(t, discovery, opts, workload, test.supported)
			if replay.Value.Relative != test.relative || replay.Value.Libdir != test.libdir ||
				replay.Value.PackageConfig != test.libdir+"/pkgconfig" || replay.Value.Defined != "yes" ||
				replay.Value.Downstream != test.relative+":"+test.libdir+"/pkgconfig" ||
				replay.Value.Origin != "file" || replay.Value.Flavor != "recursive" {
				t.Fatalf("LP64 recursive assignment replay = %#v, want relative %q under %q with file/recursive identity", replay.Value, test.relative, test.libdir)
			}
		})
	}
}

func TestKbuildComplementaryRecursiveAssignmentStateNormalizes(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct{ Value, Origin, Flavor string }
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables: map[string]string{"CC": opts.Target.Tools["cc"]}, ConfigVariablesComplete: true,
			MakeVariablesComplete: true, CaptureVariables: []string{"choice", "choice_origin", "choice_flavor"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(symbolicHardeningCompilerFixture+`
ifeq ($(first),-ffirst)
choice = lib64
else
choice = lib
endif
choice ?= wrong
choice += tail
choice_origin := $(origin choice)
choice_flavor := $(flavor choice)
`), "symbolic/recursive-normalized.mk", options, "")
		if err != nil {
			return result{}, err
		}
		return result{Value: parsed.Variables["choice"], Origin: parsed.Variables["choice_origin"], Flavor: parsed.Variables["choice_flavor"]}, nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.Origin != "file" || discovery.Value.Flavor != "recursive" {
		t.Fatalf("normalized recursive assignment identity = %#v", discovery.Value)
	}
	for _, test := range []struct {
		name, value string
		supported   bool
	}{
		{name: "selected", supported: true, value: "lib64 tail"},
		{name: "fallback", supported: false, value: "lib tail"},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay := replayKbuildSymbolicStateEvaluation(t, discovery, opts, workload, test.supported)
			if replay.Value.Value != test.value || replay.Value.Origin != "file" || replay.Value.Flavor != "recursive" {
				t.Fatalf("normalized recursive assignment replay = %#v, want %q file/recursive", replay.Value, test.value)
			}
		})
	}
}

func TestKbuildGuardedRecursiveAssignmentPreservesFrozenSimplePath(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct{ Value, Snapshot, Defined, Origin, Flavor string }
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables: map[string]string{"CC": opts.Target.Tools["cc"]}, ConfigVariablesComplete: true,
			MakeVariablesComplete: true,
			CaptureVariables:      []string{"choice", "snapshot", "choice_defined", "choice_origin", "choice_flavor"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(symbolicHardeningCompilerFixture+`
choice := $$cash
ifneq ($(first),)
choice = recursive
endif
snapshot := $(choice)
ifdef choice
choice_defined := yes
endif
choice_origin := $(origin choice)
choice_flavor := $(flavor choice)
`), "symbolic/recursive-over-simple.mk", options, "")
		if err != nil {
			return result{}, err
		}
		return result{
			Value: parsed.Variables["choice"], Snapshot: parsed.Variables["snapshot"],
			Defined: parsed.Variables["choice_defined"], Origin: parsed.Variables["choice_origin"],
			Flavor: parsed.Variables["choice_flavor"],
		}, nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.Defined != "yes" || discovery.Value.Origin != "file" {
		t.Fatalf("recursive-over-simple invariant identity = %#v, want defined/file", discovery.Value)
	}
	for name, value := range map[string]string{
		"value": discovery.Value.Value, "snapshot": discovery.Value.Snapshot, "flavor": discovery.Value.Flavor,
	} {
		if !linuxProbeSymbolPattern.MatchString(value) {
			t.Fatalf("discovery %s = %q, want retained mixed-flavor selection", name, value)
		}
	}
	for _, test := range []struct {
		name, value, flavor string
		supported           bool
	}{
		{name: "recursive replacement", supported: true, value: "recursive", flavor: "recursive"},
		{name: "frozen simple baseline", supported: false, value: "$cash", flavor: "simple"},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay := replayKbuildSymbolicStateEvaluation(t, discovery, opts, workload, test.supported)
			if replay.Value.Value != test.value || replay.Value.Snapshot != test.value ||
				replay.Value.Defined != "yes" || replay.Value.Origin != "file" || replay.Value.Flavor != test.flavor {
				t.Fatalf("recursive-over-simple replay = %#v, want value %q with flavor %q", replay.Value, test.value, test.flavor)
			}
		})
	}
}

func TestKbuildOneSidedRecursiveAssignmentRetainsIdentityUncertainty(t *testing.T) {
	_, err := evaluateSymbolicHardeningFixture(t, `
ifneq ($(first),)
choice = selected
endif
ifdef choice
observed := yes
endif
`, "observed")
	if err == nil || !strings.Contains(err.Error(), "probe-dependent definedness") {
		t.Fatalf("one-sided recursive assignment identity error = %v, want probe-dependent definedness", err)
	}
}

func TestKbuildGuardedRecursiveAssignmentRejectsDeferredValues(t *testing.T) {
	for name, value := range map[string]string{
		"parenthesized":  "$(late)",
		"braced":         "${late}",
		"single-char":    "$x",
		"escaped-dollar": "$$",
		"automatic":      "$@",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := evaluateSymbolicHardeningFixture(t, `
ifneq ($(first),)
choice = `+value+`
endif
`)
			if err == nil || !strings.Contains(err.Error(), "deferred Make expansion") {
				t.Fatalf("guarded recursive assignment %q error = %v, want deferred expansion rejection", value, err)
			}
		})
	}

	_, err := evaluateSymbolicHardeningFixture(t, `
late = before
choice = $(late)
ifneq ($(first),)
choice = selected
endif
late = after
`, "choice")
	if err == nil || !strings.Contains(err.Error(), "preserves deferred Make expansion") {
		t.Fatalf("preserved recursive late binding error = %v, want fail-closed rejection", err)
	}

	_, err = evaluateSymbolicHardeningFixture(t, `
choice := $(shell printf frozen)
ifneq ($(first),)
choice = selected
endif
`, "choice")
	if err == nil || !strings.Contains(err.Error(), "deferred simple expansion") {
		t.Fatalf("preserved deferred-simple error = %v, want fail-closed rejection", err)
	}
}

func TestKbuildComplementaryAppendsConvergeUndefinedVariableIdentity(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct {
		Value, Defined, Origin, Flavor, Downstream string
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"chosen", "chosen_defined", "chosen_origin", "chosen_flavor", "downstream"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(symbolicHardeningCompilerFixture+`
ifneq ($(first),)
chosen += -fselected
else
chosen += -ffallback
endif
chosen ?= -fwrong-default
ifdef chosen
chosen_defined := yes
endif
chosen_origin := $(origin chosen)
chosen_flavor := $(flavor chosen)
downstream := $(call cc-option,$(chosen))
`), "symbolic/complementary-append.mk", options, "")
		if err != nil {
			return result{}, err
		}
		return result{
			Value: parsed.Variables["chosen"], Defined: parsed.Variables["chosen_defined"], Origin: parsed.Variables["chosen_origin"],
			Flavor: parsed.Variables["chosen_flavor"], Downstream: parsed.Variables["downstream"],
		}, nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Value.Defined != "yes" || discovery.Value.Origin != "file" || discovery.Value.Flavor != "recursive" {
		t.Fatalf("complementary append identity = %#v, want file/recursive", discovery.Value)
	}
	requireKbuildSymbolicStatePlanHasNoRawAtoms(t, discovery.Plan)

	for _, test := range []struct {
		name, value string
		supported   bool
	}{
		{name: "selected", supported: true, value: "-fselected"},
		{name: "fallback", supported: false, value: "-ffallback"},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay := replayKbuildSymbolicStateEvaluation(t, discovery, opts, workload, test.supported)
			if strings.TrimSpace(replay.Value.Value) != test.value || replay.Value.Defined != "yes" ||
				replay.Value.Origin != "file" || replay.Value.Flavor != "recursive" {
				t.Fatalf("complementary append replay = %#v, want value %q with file/recursive identity", replay.Value, test.value)
			}
		})
	}
}

func TestKbuildConditionalInheritedAssignmentModelsSymbolicOriginExactly(t *testing.T) {
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	opts := KbuildProbeWorkloadOptions{Target: testKbuildProbeScopeOptions(t, fixture)}
	type result struct {
		Value, Origin, OriginFlag, Downstream string
	}
	workload := func(scopes *KbuildProbeScopes) (result, error) {
		options, err := scopes.Options("target", KbuildOptions{
			Variables:               map[string]string{"CC": opts.Target.Tools["cc"]},
			EnvironmentVariables:    map[string]string{"inherited": "-fparent"},
			ConfigVariablesComplete: true,
			MakeVariablesComplete:   true,
			CaptureVariables:        []string{"inherited", "inherited_origin", "origin_flag", "downstream"},
		})
		if err != nil {
			return result{}, err
		}
		parsed, err := parseKbuildWithOptions(strings.NewReader(symbolicHardeningCompilerFixture+`
ifneq ($(first),)
inherited += -fselected
endif
inherited_origin := $(origin inherited)
ifeq ($(inherited_origin),environment)
origin_flag := -forigin-environment
else
origin_flag := -forigin-file
endif
downstream := $(call cc-option,$(inherited) $(origin_flag))
`), "symbolic/inherited-origin.mk", options, "")
		if err != nil {
			return result{}, err
		}
		return result{
			Value: parsed.Variables["inherited"], Origin: parsed.Variables["inherited_origin"],
			OriginFlag: parsed.Variables["origin_flag"], Downstream: parsed.Variables["downstream"],
		}, nil
	}
	discovery, err := EvaluateKbuildProbeWorkload(opts, nil, workload)
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(discovery.Value.Origin) {
		t.Fatalf("discovery origin = %q, want exact retained selection", discovery.Value.Origin)
	}
	requireKbuildSymbolicStatePlanHasNoRawAtoms(t, discovery.Plan)
	for _, test := range []struct {
		name, value, origin, originFlag string
		supported                       bool
	}{
		{name: "assigned", supported: true, value: "-fparent -fselected", origin: "file", originFlag: "-forigin-file"},
		{name: "inherited", supported: false, value: "-fparent", origin: "environment", originFlag: "-forigin-environment"},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay := replayKbuildSymbolicStateEvaluation(t, discovery, opts, workload, test.supported)
			if strings.TrimSpace(replay.Value.Value) != test.value || replay.Value.Origin != test.origin ||
				replay.Value.OriginFlag != test.originFlag {
				t.Fatalf("inherited origin replay = %#v, want value %q origin %q flag %q", replay.Value, test.value, test.origin, test.originFlag)
			}
		})
	}
}
