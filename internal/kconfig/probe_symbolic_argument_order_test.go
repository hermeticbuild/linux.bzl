package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestProbeSymbolicArgumentLoweringAnchorsDynamicTextInSourceOrder(t *testing.T) {
	textToken := linuxProbeSymbolPrefix + strings.Repeat("1", 64)
	finiteToken := linuxProbeSymbolPrefix + strings.Repeat("2", 64)
	textReference := ProbeReference{
		NodeID: strings.Repeat("3", 64), RequestID: strings.Repeat("4", 64), Scope: "host", Kind: "text",
	}
	finiteReference := ProbeReference{
		NodeID: strings.Repeat("5", 64), RequestID: strings.Repeat("6", 64), Scope: "host", Kind: "boolean",
	}

	tests := []struct {
		name              string
		arguments         []string
		wantBase          []string
		wantConditionalAt int
		wantFragmentAt    []int
		wantDependencies  []ProbeReference
		wantTextOrdinal   string
	}{
		{
			name: "finite before text", arguments: []string{finiteToken, textToken, "tail"},
			wantBase: []string{"", "tail"}, wantConditionalAt: 0, wantFragmentAt: []int{0},
			wantDependencies: []ProbeReference{finiteReference, textReference}, wantTextOrdinal: "00000001",
		},
		{
			name: "text before finite", arguments: []string{textToken, finiteToken, "tail"},
			wantBase: []string{"", "tail"}, wantConditionalAt: 1, wantFragmentAt: []int{0},
			wantDependencies: []ProbeReference{textReference, finiteReference}, wantTextOrdinal: "00000000",
		},
		{
			name: "adjacent repeated text", arguments: []string{textToken, textToken, "tail"},
			wantBase: []string{"", "", "tail"}, wantConditionalAt: -1, wantFragmentAt: []int{0, 1},
			wantDependencies: []ProbeReference{textReference}, wantTextOrdinal: "00000000",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evaluator := &LinuxProbeEvaluator{
				scope: "host",
				symbols: map[string]linuxProbeSymbol{
					textToken: {
						kind: "text", reference: textReference,
					},
					finiteToken: {
						kind: "boolean", reference: finiteReference,
						trueText: "finite", falseText: "",
					},
				},
				symbolRegistry: newLinuxProbeSymbolRegistry(),
			}
			lowerer := newProbeSymbolicValueLowerer(evaluator)
			base, conditional, argumentFragments, err := lowerer.arguments(test.arguments)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(base, test.wantBase) {
				t.Fatalf("base arguments = %q, want %q", base, test.wantBase)
			}
			if !slices.Equal(lowerer.dependencies, test.wantDependencies) {
				t.Fatalf("dependencies = %#v, want %#v", lowerer.dependencies, test.wantDependencies)
			}
			if len(argumentFragments) != len(test.wantFragmentAt) {
				t.Fatalf("argument fragments = %#v, want indexes %v", argumentFragments, test.wantFragmentAt)
			}
			for index, wantIndex := range test.wantFragmentAt {
				group := argumentFragments[index]
				if group.Index != wantIndex || len(group.Fragments) != 1 ||
					group.Fragments[0].Value != "${result:"+test.wantTextOrdinal+".text}" {
					t.Fatalf("argument fragment %d = %#v, want index %d and text ordinal %s", index, group, wantIndex, test.wantTextOrdinal)
				}
			}
			if test.wantConditionalAt < 0 {
				if len(conditional) != 0 {
					t.Fatalf("conditional arguments = %#v, want none", conditional)
				}
				return
			}
			if len(conditional) != 1 || conditional[0].Before != test.wantConditionalAt ||
				!slices.Equal(conditional[0].Arguments, []string{"finite"}) {
				t.Fatalf("conditional arguments = %#v, want finite at %d", conditional, test.wantConditionalAt)
			}
		})
	}
}
