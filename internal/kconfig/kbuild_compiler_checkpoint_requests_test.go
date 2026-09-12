package kconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
)

func TestCompilerCheckpointInternsLargeProgramsAcrossRequestContexts(t *testing.T) {
	options := compilerDefinednessTestOptions(t)
	scopes := compilerGuardBatchScopesForTest(t, options)
	program := "/*" + strings.Repeat("source-owned-program", 45000) + "*/\n"
	for index := 0; index < 80; index++ {
		if _, err := scopes.evaluators["target"].requestText(ProbeRequest{Schema: LinuxProbeRequestSchema,
			Steps:   []ProbeStep{{Name: "measure", Tool: "cc", Stdin: program, Arguments: []string{"-E", "-x", "c", "-"}, Environment: map[string]string{"CONTEXT": fmt.Sprint(index)}}},
			Outcome: ProbeOutcome{Kind: "text", Step: "measure", Stream: "stdout", RequireSuccess: true}}); err != nil {
			t.Fatal(err)
		}
	}
	before := compilerGuardBatchOrdinaryPlanForTest(t, scopes)
	unpacked, err := json.Marshal(before)
	if err != nil || len(unpacked) <= MaxKbuildCompilerCheckpointBytes {
		t.Fatalf("fixture does not exceed the old wire limit: bytes=%d err=%v", len(unpacked), err)
	}
	data, err := scopes.MarshalActionPlanCompilerCheckpoint([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= 2<<20 {
		t.Fatalf("large program repeated in checkpoint: %d bytes", len(data))
	}
	if !reflect.DeepEqual(before, compilerGuardBatchOrdinaryPlanForTest(t, scopes)) {
		t.Fatal("packing mutated the live producer request graph")
	}
	fresh := compilerGuardBatchScopesForTest(t, options)
	if err := fresh.RestoreCompilerCheckpoint(data, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, compilerGuardBatchOrdinaryPlanForTest(t, fresh)) {
		t.Fatal("program dictionary changed request bytes, IDs, contexts or inputs")
	}
	again, err := fresh.MarshalActionPlanCompilerCheckpoint([]byte(`{}`))
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("program dictionary round trip is not canonical: %v", err)
	}
}

func TestCompilerCheckpointRejectsMalformedProgramDictionaries(t *testing.T) {
	program := strings.Repeat("A", compilerCheckpointStdinThreshold)
	base := func() kbuildCompilerCheckpoint {
		return kbuildCompilerCheckpoint{Plan: &ProbePlan{Requests: map[string]ProbeRequest{"r": {Steps: []ProbeStep{{}}}}},
			StdinPrograms: []string{program}, StdinReferences: map[string][]int{"r": {0}}}
	}
	for name, mutate := range map[string]func(*kbuildCompilerCheckpoint){
		"missing table":     func(r *kbuildCompilerCheckpoint) { r.StdinPrograms = nil },
		"unused program":    func(r *kbuildCompilerCheckpoint) { r.StdinPrograms = append(r.StdinPrograms, strings.Repeat("B", 256)) },
		"duplicate program": func(r *kbuildCompilerCheckpoint) { r.StdinPrograms = append(r.StdinPrograms, program) },
		"unknown request":   func(r *kbuildCompilerCheckpoint) { r.StdinReferences["unknown"] = []int{0} },
		"wrong extent":      func(r *kbuildCompilerCheckpoint) { r.StdinReferences["r"] = []int{0, 0} },
		"negative index":    func(r *kbuildCompilerCheckpoint) { r.StdinReferences["r"][0] = -2 },
		"out of range":      func(r *kbuildCompilerCheckpoint) { r.StdinReferences["r"][0] = 1 },
		"empty binding":     func(r *kbuildCompilerCheckpoint) { r.StdinReferences["r"][0] = -1 },
		"second payload":    func(r *kbuildCompilerCheckpoint) { r.Plan.Requests["r"].Steps[0].Stdin = "inline" },
		"unbound program": func(r *kbuildCompilerCheckpoint) {
			r.StdinReferences = map[string][]int{}
			r.Plan.Requests["r"].Steps[0].Stdin = program
		},
		"invalid utf8": func(r *kbuildCompilerCheckpoint) { r.StdinPrograms[0] = program + "\xff" },
		"nul program":  func(r *kbuildCompilerCheckpoint) { r.StdinPrograms[0] = program + "\x00" },
		"oversized program": func(r *kbuildCompilerCheckpoint) {
			r.StdinPrograms[0] = strings.Repeat("A", MaxProbeInterpolatedBytes+1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := base()
			mutate(&record)
			before, _ := json.Marshal(record)
			if err := unpackCompilerCheckpointStdin(&record); err == nil {
				t.Fatal("malformed program dictionary accepted")
			}
			after, _ := json.Marshal(record)
			if !bytes.Equal(before, after) {
				t.Fatal("failed validation partially rebound requests")
			}
		})
	}
	// A tiny table cannot make validation/hash work grow without a bound.
	record := base()
	record.StdinPrograms[0] = strings.Repeat("A", MaxProbeInterpolatedBytes)
	for index := 0; index <= MaxActionPlanSnapshotBytes/MaxProbeInterpolatedBytes; index++ {
		id := fmt.Sprint(index)
		record.Plan.Requests[id] = ProbeRequest{Steps: []ProbeStep{{}}}
		record.StdinReferences[id] = []int{0}
	}
	before := maps.Clone(record.Plan.Requests)
	if err := unpackCompilerCheckpointStdin(&record); err == nil || !strings.Contains(err.Error(), "expanded plan budget") {
		t.Fatalf("dictionary amplification not rejected: %v", err)
	}
	if !reflect.DeepEqual(before, record.Plan.Requests) {
		t.Fatal("amplification rejection mutated requests")
	}
}

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
