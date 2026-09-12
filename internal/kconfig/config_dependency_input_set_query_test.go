package kconfig

import (
	"fmt"
	"strings"
	"testing"
)

func TestConfigDependencyInputSetQueryMatchesNaiveProvenanceAndUses(t *testing.T) {
	for _, test := range []struct {
		name, source, producer, pathname string
		slot                             int
	}{
		{name: "ordinary source", source: "src-00000001"},
		{name: "config source", source: "src-00000002"},
		{name: "capsule source", source: "src-00000003"},
		{name: "ordinary producer", producer: "ordinary"},
		{name: "fallback producer", producer: "fallback"},
		{name: "unknown source", source: "src-99999999"},
		{name: "unknown producer", producer: "missing"},
		{name: "invalid producer slot", producer: "fallback", slot: 1},
		{name: "path match bypasses source error only for staging", source: "src-99999999", pathname: "include/generated/autoconf.h"},
	} {
		for _, flags := range []string{"unused", "compiler", "auxiliary"} {
			t.Run(test.name+"/"+flags, func(t *testing.T) {
				plan := configDependencyInputSetQueryTestPlan()
				pathname := test.pathname
				if pathname == "" {
					pathname = "input.h"
				}
				entry := ActionPlanInputSetEntry{
					Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: pathname},
					SourceID: test.source, ProducerID: test.producer, Slot: test.slot,
					CompilerUse: flags != "unused", AuxiliaryUse: flags == "auxiliary",
				}
				root := actionPlanInputSetTestInsert(t, plan.inputSetStore, "", []ActionPlanInputSetEntry{entry})
				assertConfigDependencyInputSetQueryMatchesNaive(t, plan, root)
			})
		}
	}
}

func TestConfigDependencyInputSetQueryPreservesLexicalEvents(t *testing.T) {
	for _, source := range []bool{false, true} {
		for _, firstError := range []bool{false, true} {
			t.Run(fmt.Sprintf("source=%v/firstError=%v", source, firstError), func(t *testing.T) {
				plan := configDependencyInputSetQueryTestPlan()
				first := ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "a-config.h"}
				last := ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "z-config.h"}
				// Force the lexically first event into a later radix branch. A
				// reducer that returns its first trie-order result must be wrong.
				for index := 0; ; index++ {
					first.Path = fmt.Sprintf("a-%04d.h", index)
					if actionPlanInputSetTargetNibble(first, 0) > actionPlanInputSetTargetNibble(last, 0) {
						break
					}
					if index == 1000 {
						t.Fatal("could not construct reverse radix/lexical event order")
					}
				}
				entries := make([]ActionPlanInputSetEntry, 48)
				for index := range entries {
					entries[index] = ActionPlanInputSetEntry{
						Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: fmt.Sprintf("middle-%04d.h", index)},
						SourceID: "src-00000001",
					}
				}
				for index, target := range []ActionPlanInputSetTarget{first, last} {
					entry := ActionPlanInputSetEntry{Target: target, CompilerUse: true, AuxiliaryUse: true}
					isError := (index == 0) == firstError
					if source {
						entry.SourceID = "src-00000002"
						if isError {
							entry.SourceID = "src-99999999"
						}
					} else {
						entry.ProducerID = "fallback"
						if isError {
							entry.ProducerID = "missing"
						}
					}
					entries = append(entries, entry)
				}
				root := actionPlanInputSetTestInsert(t, plan.inputSetStore, "", entries)
				assertConfigDependencyInputSetQueryMatchesNaive(t, plan, root)
				query := newConfigDependencyInputSetQuery(plan)
				if staged, err := query.stagesConfig(root); err == nil || staged {
					t.Fatalf("staging must validate provenance even after a match: %v, %v", staged, err)
				}
				marker, used, err := query.configUse(root, source, true)
				if firstError {
					if err == nil || used {
						t.Fatalf("early error was hidden by a later match: %q, %v, %v", marker, used, err)
					}
				} else if err != nil || !used || marker != actionPlanInputSetTargetDescription(first) {
					t.Fatalf("early match did not hide later provenance error: %q, %v, %v", marker, used, err)
				}
			})
		}
	}
}

func TestConfigDependencyInputSetQueryRejectsMalformedRoots(t *testing.T) {
	for _, name := range []string{"missing root", "invalid digest", "non-root depth", "missing child"} {
		t.Run(name, func(t *testing.T) {
			plan := configDependencyInputSetQueryTestPlan()
			entries := make([]ActionPlanInputSetEntry, 48)
			for index := range entries {
				entries[index] = ActionPlanInputSetEntry{
					Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: fmt.Sprintf("config-%04d.h", index)},
					SourceID: "src-00000002", CompilerUse: true, AuxiliaryUse: true,
				}
			}
			root := actionPlanInputSetTestInsert(t, plan.inputSetStore, "", entries)
			node := plan.inputSetStore.nodes[root]
			switch name {
			case "missing root":
				root = actionPlanInputSetTestDigest("missing root")
			case "invalid digest":
				root = "not-a-digest"
			case "non-root depth":
				node.Depth = 1
				plan.inputSetStore.nodes[root] = node
			case "missing child":
				// Config matches in earlier children cannot hide a collection error.
				delete(plan.inputSetStore.nodes, node.Children[len(node.Children)-1].ID)
			}
			assertConfigDependencyInputSetQueryMatchesNaive(t, plan, root)
			if _, _, err := newConfigDependencyInputSetQuery(plan).configUse(root, true, true); err == nil {
				t.Fatal("malformed root accepted")
			}
		})
	}
	t.Run("cycle", func(t *testing.T) {
		plan := configDependencyInputSetQueryTestPlan()
		root := actionPlanInputSetTestDigest("cycle")
		plan.inputSetStore.nodes[root] = ActionPlanInputSetNode{
			Kind: ActionPlanInputSetBranchNode, Children: []ActionPlanInputSetChild{{Nibble: "0", ID: root}},
		}
		query := newConfigDependencyInputSetQuery(plan)
		if _, err := query.stagesConfig(root); err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("cycle error = %v", err)
		}
		if _, _, err := query.configUse(root, true, true); err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("cached cycle error = %v", err)
		}
	})
}

func TestConfigDependencyInputSetQueryMemoizesUniqueSubtrees(t *testing.T) {
	plan := configDependencyInputSetQueryTestPlan()
	query := newConfigDependencyInputSetQuery(plan)
	var roots []string
	root := ""
	for index := 0; index < 128; index++ {
		root = actionPlanInputSetTestInsert(t, plan.inputSetStore, root, []ActionPlanInputSetEntry{{
			Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: fmt.Sprintf("input-%04d.h", index)},
			SourceID: "src-00000002", CompilerUse: true, AuxiliaryUse: true,
		}})
		roots = append(roots, root)
	}
	check := func() {
		for _, root := range roots {
			if staged, err := query.stagesConfig(root); err != nil || !staged {
				t.Fatalf("stagesConfig = %v, %v", staged, err)
			}
			for _, source := range []bool{false, true} {
				for _, scanAll := range []bool{false, true} {
					if _, used, err := query.configUse(root, source, scanAll); err != nil || used != source {
						t.Fatalf("configUse = %v, %v, want used=%v", used, err, source)
					}
				}
			}
		}
	}
	check()
	reachable, err := plan.inputSetStore.ReachableNodesForRoots(roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(query.summaries) != len(reachable) {
		t.Fatalf("memo contains %d nodes, want %d unique reachable nodes", len(query.summaries), len(reachable))
	}
	// Cached queries must not resolve provenance again. Removing the analysis
	// plan here makes any accidental repeated leaf evaluation fail visibly.
	query.plan = nil
	check()
}

func TestConfigDependencyInputSetQueryIsSnapshotScopedAndLazy(t *testing.T) {
	query := newConfigDependencyInputSetQuery(nil)
	if staged, err := query.stagesConfig(""); staged || err != nil || query.storeLoaded {
		t.Fatalf("empty root loaded the store: staged=%v err=%v", staged, err)
	}
	plan := configDependencyInputSetQueryTestPlan()
	root := actionPlanInputSetTestInsert(t, plan.inputSetStore, "", []ActionPlanInputSetEntry{{
		Target:   ActionPlanInputSetTarget{Kind: ActionPlanInputSetWorkTarget, Path: "input.h"},
		SourceID: "src-99999999", CompilerUse: true,
	}})
	if _, err := newConfigDependencyInputSetQuery(plan).stagesConfig(root); err == nil {
		t.Fatal("missing provenance accepted")
	}
	plan.Sources = append(plan.Sources, ActionPlanSource{ID: "src-99999999", Namespace: "config", Path: "auto.conf"})
	if staged, err := newConfigDependencyInputSetQuery(plan).stagesConfig(root); err != nil || !staged {
		t.Fatalf("fresh query retained a previous snapshot error: %v, %v", staged, err)
	}
}

func configDependencyInputSetQueryTestPlan() *ActionPlan {
	return &ActionPlan{
		inputSetStore: newPlanningActionPlanInputSetStore(),
		Sources: []ActionPlanSource{
			{ID: "src-00000001", Namespace: "kernel", Path: "ordinary.h"},
			{ID: "src-00000002", Namespace: "config", Path: "autoconf.h"},
			{ID: "src-00000003", Namespace: "capsule", Path: "capsule/include/generated/autoconf.h"},
		},
		Nodes: []ActionPlanNode{
			{ID: "ordinary", Outputs: []ActionPlanOutput{{Tree: "objects", Path: "ordinary.o"}}},
			{ID: "fallback", Stage: "prep", Kind: "copy", Tool: "actionfile", Product: "sdk", Recipe: "copy",
				Sources: []ActionPlanSourceEdge{{Role: "input", SourceID: "src-00000002"}},
				Outputs: []ActionPlanOutput{{Tree: "prep", Path: "include/generated/autoconf.h"}}},
		},
		Recipes: map[string]ActionRecipe{"copy": {
			Schema: LinuxKernelPlanSchema, Kind: "copy", Tool: "actionfile",
			Arguments: []string{"-input", "${source:input:00000000}", "-out", "${output:00000000}"},
			Sources:   []string{"input:00000000"}, Outputs: []string{"00000000"},
		}},
	}
}

// These intentionally retain the original flatten/sort/Walk callback semantics
// independently of the production query wrappers.
func naiveConfigDependencyInputSetQuery(plan *ActionPlan, root string, stage, source, scanAll bool) (string, bool, error) {
	if root == "" {
		return "", false, nil
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return "", false, err
	}
	marker, found := "", false
	err = store.Walk(root, func(entry ActionPlanInputSetEntry) error {
		if stage {
			if _, projection := configDependencyProjectionMention(entry.Target.Path); projection {
				found = true
				return nil
			}
		} else if found || !(entry.AuxiliaryUse || scanAll && entry.CompilerUse) ||
			source && entry.SourceID == "" || !source && entry.ProducerID == "" {
			return nil
		}
		provenance, err := actionPlanConfigDependencyInputSetProvenance(plan, entry)
		if err != nil {
			return err
		}
		found = found || provenance.config
		if !stage && provenance.config {
			marker = actionPlanInputSetTargetDescription(entry.Target)
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return marker, found, nil
}

func assertConfigDependencyInputSetQueryMatchesNaive(t *testing.T, plan *ActionPlan, root string) {
	t.Helper()
	query := newConfigDependencyInputSetQuery(plan)
	_, wantStaged, wantErr := naiveConfigDependencyInputSetQuery(plan, root, true, false, false)
	gotStaged, gotErr := query.stagesConfig(root)
	if gotStaged != wantStaged || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
		t.Fatalf("staging = %v, %v; naive = %v, %v", gotStaged, gotErr, wantStaged, wantErr)
	}
	for _, source := range []bool{false, true} {
		for _, scanAll := range []bool{false, true} {
			wantMarker, wantUsed, wantErr := naiveConfigDependencyInputSetQuery(plan, root, false, source, scanAll)
			gotMarker, gotUsed, gotErr := query.configUse(root, source, scanAll)
			if gotMarker != wantMarker || gotUsed != wantUsed || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Fatalf("source=%v scanAll=%v: use = %q, %v, %v; naive = %q, %v, %v",
					source, scanAll, gotMarker, gotUsed, gotErr, wantMarker, wantUsed, wantErr)
			}
		}
	}
}
