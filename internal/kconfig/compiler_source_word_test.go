package kconfig

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestActionPlanConfigProbeManyConditionalFlagsIgnoreUnrelatedSource(t *testing.T) {
	flags := make([]string, 0, 32)
	for i := range 32 {
		flags = append(flags, fmt.Sprintf("$(call cc-option,-ffixedpoint-choice-%d)", i))
	}
	for _, prerequisite := range []string{"forced.c", "forced.S", "forced.s"} {
		t.Run(prerequisite, func(t *testing.T) {
			testActionPlanConfigProbeReplayReusesCcOptionDiscoveryRequest(t,
				"$(strip "+strings.Join(flags, " ")+")", prerequisite)
		})
		t.Run(prerequisite+"/private include root", func(t *testing.T) {
			testActionPlanConfigProbeReplayReusesCcOptionDiscoveryRequest(t,
				"$(strip "+strings.Join(flags, " ")+" -I$(srctree)/include)", prerequisite)
		})
	}
}

func TestCompilerSourceWordPresenceAcrossManyConditionalFlags(t *testing.T) {
	fragments := []ProbeValueFragment{{Value: " -std=gnu11 "}}
	for i := range 32 {
		fragments = append(fragments, ProbeValueFragment{
			Value: fmt.Sprintf("-fconditional-%d ", i),
			When:  &ProbePredicate{Operator: "result-true", Result: fmt.Sprintf("%08d", i)},
		})
	}
	fragments = append(fragments, ProbeValueFragment{Value: "actual"}, ProbeValueFragment{Value: ".c "})
	group := ProbeArgumentFragments{Index: 1, Mode: ProbeArgumentFragmentsModeSourceShellWords,
		Fragments: []ProbeValueFragment{{Fragments: fragments, Transforms: []ProbeValueTransform{{
			Function: "strip", Arguments: []string{""}, InputArgument: 0,
		}}}},
	}
	if _, complete := possibleCompilerSourceWords([]string{"-nostdinc", ""}, nil, []ProbeArgumentFragments{group}); complete {
		t.Fatal("fixture no longer exceeds the complete-rendering budget")
	}
	for _, test := range []struct {
		word string
		want bool
	}{
		{"unrelated-prerequisite.c", false}, {"actual.c", true}, {"actual", false},
		{"conditional-1", false}, {"-fconditional-1", true}, {"-nostdinc", true},
	} {
		possible, complete := possibleCompilerSourceWord([]string{"-nostdinc", ""}, nil, []ProbeArgumentFragments{group}, test.word)
		if !complete || possible != test.want {
			t.Fatalf("word %q: possible=%t complete=%t, want %t", test.word, possible, complete, test.want)
		}
	}
}

func TestCompilerSourceWordPresenceWithPrivateIncludeRoot(t *testing.T) {
	for _, marker := range []string{compactKbuildActionSourceTreeMarker, compactKbuildActionObjectTreeMarker,
		compactKbuildActionAbsoluteObjectTreeMarker, compactKbuildActionHostDepsTreeMarker} {
		fragments := []ProbeValueFragment{{Value: " -std=gnu11 "}}
		for i := range 32 {
			fragments = append(fragments, ProbeValueFragment{Value: fmt.Sprintf("-fchoice-%d ", i),
				When: &ProbePredicate{Operator: "result-true", Result: fmt.Sprintf("%08d", i)}})
		}
		fragments = append(fragments, ProbeValueFragment{Value: " -I" + marker + "/lib/crc/arm"})
		groups := []ProbeArgumentFragments{{Index: 0, Mode: ProbeArgumentFragmentsModeSourceShellWords, Fragments: fragments}}
		if _, complete := possibleCompilerSourceWords([]string{""}, nil, groups); complete {
			t.Fatal("fixture no longer exceeds the exact renderer's budget")
		}
		if possible, complete := possibleCompilerSourceWord([]string{""}, nil, groups, "scripts/recordmcount.c"); possible || !complete {
			t.Fatalf("private include root retained unrelated source: possible=%t complete=%t", possible, complete)
		}
		include := "-I" + compactKbuildMaterializeActionTreeMarkers(marker) + "/lib/crc/arm"
		if possible, complete := possibleCompilerSourceWord([]string{""}, nil, groups, include); !possible || !complete {
			t.Fatal("lost the materialized include word")
		}
	}
}

func TestCompilerSourceWordPrivateRootsMatchSourceShellParser(t *testing.T) {
	marker := compactKbuildActionSourceTreeMarker
	condition := &ProbePredicate{Operator: "result-true", Result: "00000000"}
	for _, prefix := range []string{" -I", "prefix", " "} {
		for _, suffix := range []string{"", "/file.c", " "} {
			groups := []ProbeArgumentFragments{{Index: 0, Mode: ProbeArgumentFragmentsModeSourceShellWords,
				Fragments: []ProbeValueFragment{{Value: prefix + marker + suffix}, {Value: " tail.c", When: condition}}}}
			words, complete := possibleCompilerSourceWords([]string{""}, nil, groups)
			if !complete {
				t.Fatal("small source-shell oracle failed")
			}
			for _, word := range append(slices.Collect(maps.Keys(words)), "missing.c") {
				possible, complete := possibleCompilerSourceWord([]string{""}, nil, groups, word)
				if !complete || possible != words[word] {
					t.Fatalf("root spelling %q/%q, word %q: possible=%t complete=%t oracle=%t", prefix, suffix, word, possible, complete, words[word])
				}
			}
		}
	}
	for _, fragments := range [][]ProbeValueFragment{
		{{Value: marker + "/file.c"}},
		{{Value: "prefix/"}, {Value: compactKbuildActionObjectTreeMarker + "/file.c"}},
		{{Value: " " + marker + "/"}, {Value: compactKbuildActionObjectTreeMarker + "/file.c"}},
		{{Value: " " + marker + "/" + compactKbuildActionObjectTreeMarker + "/file.c"}},
		{{Value: " -I\x01unknown\x02/file.c"}},
		{{Value: " -I\x01linux-bzl-action-"}, {Value: "source-tree\x02/file.c"}},
	} {
		groups := []ProbeArgumentFragments{{Index: 0, Mode: ProbeArgumentFragmentsModeSourceShellWords, Fragments: fragments}}
		if possible, complete := possibleCompilerSourceWord([]string{""}, nil, groups, "missing.c"); !possible || complete {
			t.Fatal("joining or malformed root acquired an absence proof")
		}
	}
	groups := []ProbeArgumentFragments{{Index: 0, Fragments: []ProbeValueFragment{{Value: " -I" + marker + "/file.c"}}}}
	if possible, complete := possibleCompilerSourceWord([]string{""}, nil, groups, "missing.c"); !possible || complete {
		t.Fatal("ordinary field splitting gained source-shell marker semantics")
	}
}

func TestCompilerSourceWordPresenceMatchesExhaustiveSmallLanguages(t *testing.T) {
	condition := &ProbePredicate{Operator: "result-true", Result: "00000000"}
	literals := []string{"", "a", "b", "ab", " ", " a", "b ", "a b", "/"}
	for _, a := range literals {
		for _, b := range literals {
			for _, c := range literals {
				fragments := []ProbeValueFragment{{Value: a}, {Value: b, When: condition}, {Fragments: []ProbeValueFragment{{Value: c, When: condition}}}}
				group := []ProbeArgumentFragments{{Index: 0, Mode: ProbeArgumentFragmentsModeSourceShellWords, Fragments: fragments}}
				words, complete := possibleCompilerSourceWords([]string{""}, nil, group)
				if !complete {
					t.Fatal("small oracle language is unexpectedly unsupported")
				}
				for _, candidate := range []string{"a", "b", "ab", "a/b", "ba"} {
					possible, complete := possibleCompilerSourceWord([]string{""}, nil, group, candidate)
					if !complete || possible != words[candidate] {
						t.Fatalf("parts=%q/%q/%q candidate=%q: possible=%t complete=%t oracle=%t", a, b, c, candidate, possible, complete, words[candidate])
					}
				}
			}
		}
	}
}

func TestCompilerSourceWordPresenceFailsClosed(t *testing.T) {
	strip := ProbeValueTransform{Function: "strip", Arguments: []string{""}, InputArgument: 0}
	cases := [][]ProbeValueFragment{
		{{Value: "'source.c'"}},
		{{Value: "source\\.c"}},
		{{Value: "${result:00000000.text}"}},
		{{Value: "$(command)"}},
		{{Value: "source.c; command"}},
		{{Value: "src/*.c"}},
		{{Value: "source.c\u00a0"}},
		{{Value: "a"}, {Fragments: []ProbeValueFragment{{Value: " b "}}, Transforms: []ProbeValueTransform{strip}}, {Value: "c"}},
		{{Value: "source.o", Transforms: []ProbeValueTransform{{Function: "subst", Arguments: []string{".o", ".c", ""}, InputArgument: 2}}}},
	}
	for _, fragments := range cases {
		possible, complete := possibleCompilerSourceWord([]string{""}, nil, []ProbeArgumentFragments{{Index: 0, Mode: ProbeArgumentFragmentsModeSourceShellWords, Fragments: fragments}}, "source.c")
		if !possible || complete {
			t.Fatalf("unsupported fragments acquired absence proof: %#v", fragments)
		}
	}
	for _, candidate := range []string{"", strings.Repeat("a", maxCompilerSourceWordLength+1)} {
		if possible, complete := possibleCompilerSourceWord(nil, nil, nil, candidate); !possible || complete {
			t.Fatal("invalid/budgeted word acquired absence proof")
		}
	}
	deep := []ProbeValueFragment{{Value: "source.c"}}
	for range MaxProbeValueFragmentDepth + 2 {
		deep = []ProbeValueFragment{{Fragments: deep}}
	}
	if possible, complete := possibleCompilerSourceWord([]string{""}, nil, []ProbeArgumentFragments{{Index: 0, Fragments: deep}}, "other.c"); !possible || complete {
		t.Fatal("depth budget acquired absence proof")
	}
	machine := compilerSourceWordMachine{word: "source.c", work: maxCompilerSourceWordWork}
	if possible, complete := machine.group(ProbeArgumentFragments{Fragments: []ProbeValueFragment{{Value: "other.c"}}}); !possible || complete {
		t.Fatal("work budget acquired absence proof")
	}
}

func TestCompilerSourceWordPresenceRetainsEveryArgumentGroup(t *testing.T) {
	base := []string{"-D", "scalar.c", "replaced.c"}
	conditional := []ProbeConditionalArguments{{Before: 0, Arguments: []string{"conditional.S"}}}
	groups := []ProbeArgumentFragments{{Index: 2, Mode: ProbeArgumentFragmentsModeSourceShellWords, Fragments: []ProbeValueFragment{{Value: "fragment.cc"}}}}
	for _, word := range []string{"scalar.c", "conditional.S", "fragment.cc"} {
		if possible, complete := possibleCompilerSourceWord(base, conditional, groups, word); !possible || !complete {
			t.Fatalf("lost argument %q", word)
		}
	}
	if possible, complete := possibleCompilerSourceWord(base, conditional, groups, "replaced.c"); possible || !complete {
		t.Fatal("overridden base slot was treated as an argument")
	}
	before := slices.Clone(base)
	_, _ = possibleCompilerSourceWord(base, conditional, groups, "missing.c")
	if !slices.Equal(base, before) {
		t.Fatal("modified source argv")
	}
}
