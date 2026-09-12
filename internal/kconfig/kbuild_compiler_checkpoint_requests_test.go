package kconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestCompilerCheckpointInternsExactRequestState(t *testing.T) {
	request := ProbeRequest{Schema: LinuxProbeRequestSchema, Sources: []string{}, Steps: []ProbeStep{{Name: "unique-request-program", Arguments: []string{}, Environment: map[string]string{}}}}
	ref := ProbeReference{NodeID: "node", RequestID: "request", Scope: "target", Kind: "text"}
	definition := kbuildCheckpointDefinition{ref, request, []ProbeReference{}}
	original := kbuildCompilerCheckpoint{Plan: &ProbePlan{Requests: map[string]ProbeRequest{"request": request}}, Definitions: map[string]kbuildCheckpointDefinition{"node": definition}, Symbols: map[string]kbuildCheckpointSymbol{}}
	for index := 0; index < 1024; index++ {
		original.Symbols[fmt.Sprint(index)] = kbuildCheckpointSymbol{Definition: definition, SelectionInputs: []kbuildCheckpointDefinition{definition}}
	}
	if err := bindCompilerCheckpointRequests(&original, true); err != nil {
		t.Fatal(err)
	}
	var err error
	original.EmptyContainers, err = compilerCheckpointEmptyContainers(original)
	if err != nil || len(original.EmptyContainers) != 3 {
		t.Fatalf("request shape must be recorded only once: %#v, %v", original.EmptyContainers, err)
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte("unique-request-program")) != 1 {
		t.Fatal("wire format duplicates request bytes per definition or symbol")
	}
	var restored kbuildCompilerCheckpoint
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if err := restoreCompilerCheckpointEmptyContainers(&restored); err != nil {
		t.Fatal(err)
	}
	if err := bindCompilerCheckpointRequests(&restored, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, original) {
		t.Fatal("interning changed exact request/reference/dependency shape")
	}
	broken := restored.Symbols["0"]
	broken.Definition.Request.Schema = "different"
	restored.Symbols["0"] = broken
	if err := bindCompilerCheckpointRequests(&restored, true); err == nil {
		t.Fatal("capture discarded a contradictory repeated request")
	}
	broken.Definition.Reference.RequestID = "missing"
	restored.Symbols["0"] = broken
	if err := bindCompilerCheckpointRequests(&restored, false); err == nil {
		t.Fatal("restore invented a request missing from the interned plan")
	}
}

func TestActionPlanCompilerCheckpointRetainsOnlyTransitiveSymbols(t *testing.T) {
	options, original, normalizer, _ := compilerCheckpointKindsFixture(t)
	var root string
	for token, symbol := range original.evaluators["target"].symbolRegistry.symbols {
		if symbol.kind == "make-text" {
			root = token
		}
	}
	if root == "" {
		t.Fatal("fixture has no Make expression")
	}
	payload, _ := json.Marshal(map[string]string{"arbitrary_nested_action_field": root})
	data, err := original.MarshalActionPlanCompilerCheckpoint(payload)
	if err != nil {
		t.Fatal(err)
	}
	var record kbuildCompilerCheckpoint
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Symbols) != 2 || record.Symbols[root].Kind != "make-text" {
		t.Fatalf("did not retain just the Make expression and its text dependency: %#v", record.Symbols)
	}
	fresh := compilerGuardBatchScopesForTest(t, options)
	if err := fresh.RestoreCompilerCheckpoint(data, normalizer(true)); err != nil {
		t.Fatal(err)
	}
	again, err := fresh.MarshalActionPlanCompilerCheckpoint(payload)
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("closure replay changed exact retained compiler state: %v", err)
	}
	// Capture must not mutate the live producer's complete registry.
	if len(original.evaluators["target"].symbolRegistry.symbols) != 7 {
		t.Fatal("checkpoint pruned the producer's live symbols")
	}
}

func TestCompilerCheckpointSymbolClosureIncludesAllIndependentRoots(t *testing.T) {
	token := func(i int) string { return linuxProbeSymbolPrefix + fmt.Sprintf("%064x", i) }
	record := kbuildCompilerCheckpoint{
		Plan:             &ProbePlan{Requests: map[string]ProbeRequest{"request": {Sources: []string{token(1)}}}},
		Scopes:           map[string]kbuildCheckpointScope{"target": {Environment: map[string]string{"VALUE": token(2)}}},
		BaseEnvironments: map[string]map[string]string{"host": {"VALUE": token(3)}},
		Symbols: map[string]kbuildCheckpointSymbol{
			token(1): {Kind: "make-text", MakeText: &kbuildCheckpointMakeText{ProtocolValue: token(5)}},
			token(2): {Kind: "transformed-text", TextTransform: &kbuildCheckpointTransform{Arguments: []string{token(6)}}},
			token(3): {Kind: "selection", SelectionValues: []string{token(7)}},
			token(4): {Kind: "boolean", TrueText: token(8)},
			token(5): {Kind: "source-shell-words", SourceShellWords: token(6)},
			token(6): {Kind: "toolset-path-literal", ToolsetPathLiteral: "current/path"},
			token(7): {Kind: "text"},
			token(8): {Kind: "text"},
			token(9): {Kind: "source-shell-words", SourceShellWords: "unreferenced intermediate"},
		},
	}
	payload, _ := json.Marshal(map[string]string{"action": token(4)})
	if err := retainCompilerCheckpointSymbols(&record, payload); err != nil {
		t.Fatal(err)
	}
	if len(record.Symbols) != 8 {
		t.Fatalf("lost a request, environment, action or nested symbol: %d", len(record.Symbols))
	}
	if _, retained := record.Symbols[token(9)]; retained {
		t.Fatal("retained an unreachable intermediate")
	}
	delete(record.Symbols, token(6))
	if err := retainCompilerCheckpointSymbols(&record, payload); err == nil || !strings.Contains(err.Error(), "unknown reachable symbol") {
		t.Fatalf("accepted an incomplete transitive closure: %v", err)
	}
}

func TestCompilerCheckpointShapeDoesNotTraverseScalarPayloads(t *testing.T) {
	// More scalar argv words than the shape work budget used to exhaust it,
	// even though none can carry a JSON-omitted container. The structural
	// budget itself is unchanged; only meaningful shape nodes are visited.
	record := kbuildCompilerCheckpoint{Plan: &ProbePlan{Requests: map[string]ProbeRequest{
		"request": {Sources: []string{}, Steps: []ProbeStep{{Arguments: make([]string, (1<<20)+1), Candidate: &ProbeCandidateArguments{Base: []int{}}}}},
	}}}
	shape, err := compilerCheckpointEmptyContainers(record)
	want := [][]string{{"Plan", "Requests", "request", "Sources"}, {"Plan", "Requests", "request", "Steps", "0", "Candidate", "Base"}}
	if err != nil || !reflect.DeepEqual(shape, want) {
		t.Fatalf("scalar payload consumed shape work: %#v, %v", shape, err)
	}
	// Actual shape-bearing structure is still bounded independently of bytes.
	record.Plan.Requests["request"] = ProbeRequest{Steps: make([]ProbeStep, 1<<17)}
	if _, err := compilerCheckpointEmptyContainers(record); err == nil {
		t.Fatal("actual structural traversal exceeded its unchanged budget")
	}
}

func TestCompilerCheckpointOmitsInactiveUnionFieldsWithoutLosingEmptySlices(t *testing.T) {
	original := kbuildCheckpointSymbol{Kind: "selection", SelectionInputs: []kbuildCheckpointDefinition{}, SelectionValues: []string{}}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("Definition")) || bytes.Contains(data, []byte("TextTransform")) || bytes.Contains(data, []byte("MakeText")) {
		t.Fatalf("inactive symbol kinds still occupy the wire: %s", data)
	}
	var restored kbuildCheckpointSymbol
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, restored) {
		t.Fatalf("compact union lost nonnil empty fields: %s", data)
	}
}
