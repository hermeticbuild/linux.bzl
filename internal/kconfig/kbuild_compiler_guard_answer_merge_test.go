package kconfig

import (
	"maps"
	"strings"
	"testing"
)

func compilerGuardAnswerMergeSnapshotForTest(t *testing.T, scopes *KbuildProbeScopes,
	arguments, units []string, environment map[string]string, names []string, stdout string,
) *KbuildCompilerGuardAnswers {
	t.Helper()
	batch, _ := compilerGuardAnswerBatchForTest(t, scopes, arguments, units, environment,
		[][]string{names}, []string{stdout})
	answers, err := batch.Answers()
	if err != nil {
		t.Fatal(err)
	}
	return answers
}

func TestMergeKbuildCompilerGuardAnswersNilAndEquivalent(t *testing.T) {
	for _, values := range [][]*KbuildCompilerGuardAnswers{nil, {nil}, {nil, nil}} {
		if got, err := MergeKbuildCompilerGuardAnswers(values...); got != nil || err != nil {
			t.Fatalf("nil merge = %#v/%v", got, err)
		}
	}
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	first := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__A"}, "0\n")
	duplicate := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__A"}, "0\n")
	for _, values := range [][]*KbuildCompilerGuardAnswers{
		{first}, {nil, first, nil}, {first, first}, {first, nil, duplicate},
	} {
		if got, err := MergeKbuildCompilerGuardAnswers(values...); got != first || err != nil {
			t.Fatalf("one effective immutable snapshot lost its identity: %#v/%v", got, err)
		}
	}
}

func TestMergeKbuildCompilerGuardAnswersOrderOwnershipAndValues(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	first := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__A"}, "0\n")
	second := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__B"}, "1\n")
	duplicate := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__A"}, "0\n")
	key := configDependencyCompilerPredefineKey("target", "cc", "c", nil, nil, nil)
	merged, err := MergeKbuildCompilerGuardAnswers(first, second)
	if err != nil || merged == first || merged == second ||
		!maps.Equal(merged.entries[key].definitions, map[string]bool{"__A": false, "__B": true}) {
		t.Fatalf("merged measured rounds = %#v/%v", merged, err)
	}
	identity := merged.entries[key].identity
	if identity == "" || identity == first.entries[key].identity || identity == second.entries[key].identity {
		t.Fatal("merged identity did not commit both contributing rounds")
	}
	for _, values := range [][]*KbuildCompilerGuardAnswers{
		{second, first}, {nil, first, duplicate, second, second}, {second, duplicate, first},
	} {
		got, err := MergeKbuildCompilerGuardAnswers(values...)
		if err != nil || got.entries[key].identity != identity || !maps.Equal(got.entries[key].definitions, merged.entries[key].definitions) {
			t.Fatalf("round order or duplicate observation changed identity: %#v/%v", got, err)
		}
	}
	// Internal mutation here deliberately violates the immutable input API to
	// check that a genuinely merged result owns its maps rather than aliasing a
	// contributor. The one-effective-snapshot fast path is intentionally shared.
	first.entries[key].definitions["__A"] = true
	delete(second.entries, key)
	first.toolsets["target"] = "changed input"
	if !maps.Equal(merged.entries[key].definitions, map[string]bool{"__A": false, "__B": true}) ||
		merged.entries[key].identity != identity || merged.toolsets["target"] == "changed input" {
		t.Fatal("input mutation changed an independently merged snapshot")
	}
	merged.entries[key].definitions["__MERGED_ONLY"] = true
	merged.toolsets["host"] = "merged only"
	if _, found := first.entries[key].definitions["__MERGED_ONLY"]; found || first.toolsets["host"] != "" {
		t.Fatal("merged snapshot storage aliases an input")
	}
}

func TestMergeKbuildCompilerGuardAnswersKeepsExactContexts(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	arguments := []string{"-DFEATURE=1", "-UFEATURE", "source.c"}
	units := []string{"source.c"}
	first := compilerGuardAnswerMergeSnapshotForTest(t, scopes, arguments, units,
		map[string]string{"MODE": "first"}, []string{"__A"}, "0\n")
	second := compilerGuardAnswerMergeSnapshotForTest(t, scopes, arguments, units,
		map[string]string{"MODE": "second"}, []string{"__A"}, "1\n")
	merged, err := MergeKbuildCompilerGuardAnswers(first, second)
	if err != nil || len(merged.entries) != 2 {
		t.Fatalf("different contexts incorrectly conflicted or collapsed: %#v/%v", merged, err)
	}
	plan := &ActionPlan{Toolsets: maps.Clone(merged.toolsets), metadata: &CompactMetadata{compilerGuardAnswers: merged}}
	for _, mode := range []string{"first", "second", "unknown"} {
		values, identity, err := configDependencySupplementalCompilerDefinedness(plan, "target", "cc", "c",
			arguments, units, map[string]string{"MODE": mode})
		if err != nil {
			t.Fatal(err)
		}
		if mode == "unknown" {
			if values != nil || identity != "" {
				t.Fatal("unknown context borrowed merged answers")
			}
		} else if identity == "" || !maps.Equal(values, map[string]bool{"__A": mode == "second"}) {
			t.Fatalf("context %s changed measured values: %v/%q", mode, values, identity)
		}
	}
}

func TestMergeKbuildCompilerGuardAnswersRejectsConflictsAndToolsets(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	first := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__A"}, "0\n")
	conflicting := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__A"}, "1\n")
	if got, err := MergeKbuildCompilerGuardAnswers(first, conflicting); got != nil || err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("contradictory measured rounds escaped: %#v/%v", got, err)
	}
	for _, change := range []string{"changed target", "missing target", "additional host"} {
		t.Run(change, func(t *testing.T) {
			other := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__B"}, "0\n")
			switch change {
			case "changed target":
				other.toolsets["target"] = "different configured target"
			case "missing target":
				delete(other.toolsets, "target")
			case "additional host":
				other.toolsets["host"] = "additional configured host"
			}
			for _, values := range [][]*KbuildCompilerGuardAnswers{{first, other}, {other, first}} {
				if got, err := MergeKbuildCompilerGuardAnswers(values...); got != nil || err == nil || !strings.Contains(err.Error(), "toolset") {
					t.Fatalf("%s accepted incomplete toolset equality: %#v/%v", change, got, err)
				}
			}
		})
	}
}

func TestMergeKbuildCompilerGuardAnswersIdentityBindsContributorsAndMeasurements(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	first := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__A"}, "0\n")
	second := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__B"}, "1\n")
	differentMeasurement := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__B"}, "0\n")
	differentQuery := compilerGuardAnswerMergeSnapshotForTest(t, scopes, nil, nil, nil, []string{"__A", "__B"}, "0 1\n")
	key := configDependencyCompilerPredefineKey("target", "cc", "c", nil, nil, nil)
	identities := map[string]bool{}
	for _, values := range [][]*KbuildCompilerGuardAnswers{
		{first, second}, {first, differentMeasurement}, {first, differentQuery},
	} {
		merged, err := MergeKbuildCompilerGuardAnswers(values...)
		if err != nil {
			t.Fatal(err)
		}
		identity := merged.entries[key].identity
		if identity == "" || identities[identity] {
			t.Fatal("changed contributing query or measurement reused a merge identity")
		}
		identities[identity] = true
	}
}
