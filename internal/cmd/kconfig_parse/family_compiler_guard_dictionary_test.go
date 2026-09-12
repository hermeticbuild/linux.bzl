package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestFamilyCompilerGuardDictionaryExactRoundTrip(t *testing.T) {
	contexts := map[string]familyCompilerGuardContext{}
	for _, c := range []familyCompilerGuardContext{
		{}, {Arguments: []string{}, TranslationUnits: []string{}, Environment: map[string]string{}},
		{Scope: "target", Role: "cc", Language: "c", Arguments: []string{"-DX=1", "-UX", "-DX=2", "-DQUOTED=\"one two\"", "unicode-λ", "line\n\x00<>&\u2028"}, Environment: map[string]string{"Z": "", "A": "space value"}},
	} {
		contexts[c.id()] = c
	}
	words, packed := packFamilyCompilerGuardContexts(contexts)
	got, err := unpackFamilyCompilerGuardContexts(words, packed)
	if err != nil || !reflect.DeepEqual(got, contexts) {
		t.Fatalf("exact dictionary round trip: %v", err)
	}
	for _, mutation := range []string{"missing", "negative", "unused", "duplicate", "unsorted", "changed", "bad utf8", "duplicate environment", "unsorted environment"} {
		t.Run(mutation, func(t *testing.T) {
			words, packed := packFamilyCompilerGuardContexts(contexts)
			id := slices.Sorted(maps.Keys(packed))[0]
			c := packed[id]
			switch mutation {
			case "missing":
				c.Arguments = []int{len(words)}
			case "negative":
				c.Arguments = []int{-1}
			case "unused":
				words = append(words, "zzzz-unused")
			case "duplicate":
				words = append(words, words[len(words)-1])
			case "unsorted":
				slices.Reverse(words)
			case "changed":
				c.Scope = "forged"
			case "bad utf8":
				words[0] = "\xff"
			case "duplicate environment":
				c.Environment = [][2]int{{0, 1}, {0, 2}}
			case "unsorted environment":
				c.Environment = [][2]int{{1, 0}, {0, 1}}
			}
			packed[id] = c
			if _, err := unpackFamilyCompilerGuardContexts(words, packed); err == nil {
				t.Fatal("accepted changed dictionary")
			}
		})
	}
}

func TestFamilyCompilerGuardDictionaryRejectsExpansionBeforeRestoration(t *testing.T) {
	words := []string{strings.Repeat("x", 1<<20)}
	packed := map[string]familyCompilerGuardPackedContext{
		strings.Repeat("0", 64): {Arguments: make([]int, 129)},
	}
	if _, err := unpackFamilyCompilerGuardContexts(words, packed); err == nil || !strings.Contains(err.Error(), "expanded budget") {
		t.Fatalf("small reference vector bypassed unchanged expansion bound: %v", err)
	}
}

func TestFamilyCompilerGuardDictionaryLedgerBoundsEncoding(t *testing.T) {
	contexts := map[string]familyCompilerGuardContext{}
	var words map[string]struct{}
	estimate, references := 0, 0
	// Insert reverse-ordered values to force sorted dictionary indices to move.
	for i := 511; i >= 0; i-- {
		c := familyCompilerGuardContext{Scope: "target", Role: "cc", Language: "c", TranslationUnits: []string{fmt.Sprintf("unit-%d.c", i)}, Environment: map[string]string{"LANG": "C", "SHARED": strings.Repeat("environment", 20)}}
		for j := range 160 {
			c.Arguments = append(c.Arguments, fmt.Sprintf("-DKEY_%03d=%s", j, strings.Repeat("x", 64)))
		}
		c.Arguments = append(c.Arguments, fmt.Sprintf("-DUNIT=%d", i))
		before := maps.Clone(words)
		cost, added, refs := familyCompilerGuardContextCost(c, words)
		if !reflect.DeepEqual(before, words) {
			t.Fatal("preflight mutated dictionary state")
		}
		if words == nil {
			words = map[string]struct{}{}
		}
		maps.Copy(words, added)
		estimate += cost
		references += refs
		contexts[c.id()] = c
	}
	dictionary, packed := packFamilyCompilerGuardContexts(contexts)
	got, err := unpackFamilyCompilerGuardContexts(dictionary, packed)
	if err != nil || !reflect.DeepEqual(got, contexts) {
		t.Fatalf("roundtrip: %v", err)
	}
	before, _ := json.Marshal(contexts)
	after, _ := json.Marshal(struct {
		Strings  []string
		Contexts map[string]familyCompilerGuardPackedContext
	}{dictionary, packed})
	if len(after) > estimate || len(after)*4 >= len(before) || len(packed) != 512 || references != 512*166 {
		t.Fatalf("encoding/accounting mismatch: before=%d after=%d estimate=%d refs=%d", len(before), len(after), estimate, references)
	}
	t.Logf("synthetic descriptors: original=%d encoded=%d conservative_ledger=%d contexts=%d", len(before), len(after), estimate, len(packed))
}

func TestFamilyCompilerGuardDictionaryOptionalAdmissionIsTransactional(t *testing.T) {
	p := &familyCompilerGuardPipeline{bytes: maxFamilyCompilerGuardBytes / 2}
	stage := &familyCompilerGuardOptionalStage{}
	q := familyCompilerGuardTransportManifestForTest().Variants["base"][0]
	q.Kind = familyCompilerOptionalDefinednessQueryKind
	if _, admitted, err := p.admitOptionalQueryForStage(q, stage); err != nil || admitted {
		t.Fatalf("over-budget optional query admitted: %t %v", admitted, err)
	}
	if p.contextStrings != nil || p.contextReferences != 0 || p.contexts != nil || p.count != 0 || p.bytes != maxFamilyCompilerGuardBytes/2 {
		t.Fatal("rejected optional query consumed dictionary or membership budget")
	}
	p.bytes = 0
	if _, admitted, err := p.admitOptionalQueryForStage(q, stage); err != nil || !admitted {
		t.Fatalf("valid optional query rejected: %t %v", admitted, err)
	}
	words, references, compact, expanded := maps.Clone(p.contextStrings), p.contextReferences, p.bytes, p.expandedBytes
	if _, admitted, err := p.admitOptionalQueryForStage(q, stage); err != nil || !admitted {
		t.Fatalf("shared membership rejected: %t %v", admitted, err)
	}
	if !maps.Equal(words, p.contextStrings) || references != p.contextReferences || p.bytes-compact != familyCompilerGuardQueryReferenceBytes+len(q.Names[0])+4 || p.expandedBytes-expanded != familyCompilerGuardExpandedQueryBytes(q) {
		t.Fatal("sharing words discounted membership or expanded work")
	}
}

func TestFamilyCompilerGuardDictionaryRawStringBudgetCountsAliases(t *testing.T) {
	for _, field := range []string{"Strings", "strings", "sTrInGs"} {
		var data bytes.Buffer
		fmt.Fprintf(&data, "{\"%s\":[", field)
		for i := range maxFamilyCompilerGuardStrings + 1 {
			if i != 0 {
				data.WriteByte(',')
			}
			data.WriteString(`""`)
		}
		data.WriteString("]}")
		if err := preflightFamilyCompilerGuardJSON(data.Bytes()); err == nil || !strings.Contains(err.Error(), "string budget") {
			t.Fatalf("raw aliased string array escaped preflight: %v", err)
		}
	}
}
