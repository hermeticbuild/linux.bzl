package kconfig

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func counterSourceStateForTest(t *testing.T, predefines string, definitions map[string]bool) (*configDependencyMacroState, *compilerCounterSequence) {
	t.Helper()
	parsed := macroReplacementStateForTest(t, predefines)
	initial, valid := configDependencyApplyCompilerDefinedness(*parsed, definitions, "measured-definedness")
	if !valid {
		t.Fatal("invalid initial definitions")
	}
	contextID := fmt.Sprintf("%x", sha256.Sum256([]byte("exact-compiler-context")))
	sequence, err := parseCompilerCounterSequence(contextID, 4, "7 42 9 11")
	if err != nil {
		t.Fatal(err)
	}
	state := initial.branch()
	return state, sequence
}

func TestCompilerCounterNamespaceAndSourceObservations(t *testing.T) {
	definitions := map[string]bool{"__COUNTER__": true}
	const dump = "#define DROP(x)\n"
	fresh := func() *configDependencyMacroState {
		state, sequence := counterSourceStateForTest(t, dump, definitions)
		if !state.installInitialCompilerCounter(definitions, "exact-compiler-context", dump, "measured-definedness", sequence) {
			t.Fatal("initial binding rejected")
		}
		return state
	}
	t.Run("ordered observations", func(t *testing.T) {
		state := fresh()
		proof := &configDependencyCallCoverage{}
		if reason := proof.expand("DROP(__COUNTER__) __COUNTER__", state); reason != "" || state.counter.next != 1 {
			t.Fatalf("source expansion: %s %+v", reason, state.counter)
		}
		value, reason := proof.condition("__COUNTER__ == 42", state)
		if reason != "" || value != configDependencyMacroDefined || state.counter.next != 2 || len(proof.counters) != 2 {
			t.Fatalf("condition lost ordered state: %d %s %+v", value, reason, state.counter)
		}
		if value, reason := proof.condition("defined(__COUNTER__)", state); reason != "" || value != configDependencyMacroDefined || state.counter.next != 2 {
			t.Fatal("defined consumed a counter expansion")
		}
		if reason := state.applyCompilerMacroReplacements("cc", []string{"-D__COUNTER__=73"}); reason != "" {
			t.Fatal(reason)
		}
		if value, reason := proof.condition("__COUNTER__ == 73", state); reason != "" || value != configDependencyMacroDefined || state.counter.next != 2 {
			t.Fatal("textual argv replacement consumed builtin state")
		}
	})
	t.Run("post expansion failures", func(t *testing.T) {
		for _, failure := range []string{"integer expression", "coverage budget", "event boundary"} {
			state := fresh()
			proof := &configDependencyCallCoverage{}
			before, revision := state.counter, state.snapshotRevision
			var reason string
			switch failure {
			case "integer expression":
				_, reason = proof.condition("__COUNTER__ +", state)
			case "coverage budget":
				proof.work = 65536
				reason = proof.expand("__COUNTER__", state)
			case "event boundary":
				proof.priorTail = true
				reason = proof.expand("(__COUNTER__)", state)
			}
			if reason == "" || state.counter != before || state.snapshotRevision != revision || len(proof.counters) != 0 {
				t.Fatalf("%s committed a failed observation: %s %+v", failure, reason, state.counter)
			}
		}
	})
	t.Run("revocation and aliases", func(t *testing.T) {
		for _, operation := range []string{"undef", "same definedness", "source", "uncertain source", "argv undef", "numeric header"} {
			root := fresh()
			child := root.branch()
			switch operation {
			case "undef":
				child.set("__COUNTER__", configDependencyMacroUndefined)
			case "same definedness":
				child.set("__COUNTER__", configDependencyMacroDefined)
			case "source":
				child.setSourceMacroReplacement("__COUNTER__ 73", "source:1", configDependencyMacroDefined)
			case "uncertain source":
				child.setSourceMacroReplacement("__COUNTER__ 73", "source:1", configDependencyMacroUnknown)
			case "argv undef":
				child.applyCompilerMacroReplacements("cc", []string{"-U__COUNTER__"})
			case "numeric header":
				child.applyValidatedNumericMacroHeader()
			}
			if child.macroReplacements["__COUNTER__"].counter != nil || root.macroReplacements["__COUNTER__"].counter == nil || child.counter != root.counter {
				t.Fatalf("%s failed to revoke binding or changed parent/cursor", operation)
			}
		}
	})
}

func TestCompilerCounterInitialAdmissionAndCacheIsolation(t *testing.T) {
	for _, variant := range []string{"absent", "unknown", "textual", "context", "identity", "late write", "fresh"} {
		definitions := map[string]bool{"__COUNTER__": true}
		dump, context, identity := "", "exact-compiler-context", "measured-definedness"
		switch variant {
		case "absent":
			definitions["__COUNTER__"] = false
		case "unknown":
			delete(definitions, "__COUNTER__")
		case "textual":
			dump = "#define __COUNTER__ 73\n"
		case "context":
			context = "other"
		case "identity":
			identity = ""
		}
		state, sequence := counterSourceStateForTest(t, dump, definitions)
		if variant == "late write" {
			state.set("OTHER", configDependencyMacroDefined)
		}
		if accepted := state.installInitialCompilerCounter(definitions, configDependencyCompilerPredefineRequestKey(context), dump, identity, sequence); accepted != (variant == "fresh") {
			t.Fatalf("%s admission=%v", variant, accepted)
		}
		if variant != "fresh" {
			continue
		}
		if state.parent.counter != (compilerCounterCursor{}) || state.parent.macroReplacements["__COUNTER__"].counter != nil {
			t.Fatal("installation mutated cached parse root")
		}
		if state.installInitialCompilerCounter(definitions, configDependencyCompilerPredefineRequestKey(context), dump, identity, sequence) {
			t.Fatal("reinstalled or reset existing counter")
		}
		// Even if replacement state is stripped accidentally, stateful effects
		// cannot enter or hit a namespace-only cache.
		state.macroReplacements = nil
		if configDependencyMacroEffectStateReady(state) {
			t.Fatal("counter state eligible for namespace-only effect cache")
		}
		state.parent = nil
		state.compilerPredefinedSnapshot = state.snapshot
		if _, accepted := configDependencyPredefineCacheMeasure("key", dump, configDependencyCompilerPredefineParse{state: *state}, 1<<20, 1<<28); accepted {
			t.Fatal("context-bound cursor eligible for parsed predefine cache")
		}
	}
}

func TestCompilerCounterConditionalProgramIncludesAndRecursionKeys(t *testing.T) {
	definitions := map[string]bool{"__COUNTER__": true}
	state, sequence := counterSourceStateForTest(t, "", definitions)
	if !state.installInitialCompilerCounter(definitions, "exact-compiler-context", "", "measured-definedness", sequence) {
		t.Fatal("counter binding rejected")
	}
	before, valid := configDependencyConditionalMacroStateKey(state)
	if !valid {
		t.Fatal("initial recursion key rejected")
	}
	plain, valid := state.snapshot.conditionalProjection()
	if !valid || plain.counter != nil || plain.key == before.key {
		t.Fatal("source state contaminated or aliased namespace projection")
	}
	proof := &configDependencyCallCoverage{}
	childSource := "#if __COUNTER__ == 42\n#define SEEN 1\n#else\n#error wrong counter order\n#endif\n"
	parentSource := "#if 0\n__COUNTER__\n#endif\n__COUNTER__\n#include <child.h>\n__COUNTER__\n"
	childProgram := compileConfigDependencyConditionalProgramWithCalls([]byte(childSource), "child.h", true)
	parentProgram := compileConfigDependencyConditionalProgramWithCalls([]byte(parentSource), "parent.c", true)
	visited := 0
	_, reason := interpretConfigDependencyConditionalProgramWithCalls(parentProgram, state,
		func(include configDependencyLiteralInclude, active configDependencyMacroDefinition) (bool, string) {
			if include.name != "child.h" || active != configDependencyMacroDefined {
				t.Fatalf("wrong child invocation: %+v %d", include, active)
			}
			visited++
			child := state.branch()
			_, reason := interpretConfigDependencyConditionalProgramWithCalls(childProgram, child, nil, "c", proof.expand, proof.condition, proof.macroWrite)
			if reason != "" {
				return false, reason
			}
			if !state.commitBranch(child) {
				return false, "child commit failed"
			}
			return true, ""
		}, "c", proof.expand, proof.condition, proof.macroWrite)
	if reason != "" || visited != 1 || state.counter.next != 3 || state.definition("SEEN") != configDependencyMacroDefined || len(proof.counters) != 3 {
		t.Fatalf("ordered nested source failed: %s visited=%d cursor=%+v", reason, visited, state.counter)
	}
	for index, read := range proof.counters {
		if read.Ordinal != index || read.Token != sequence.values[index] {
			t.Fatalf("bad source read: %+v", read)
		}
	}
	after, valid := configDependencyConditionalMacroStateKey(state)
	if !valid || after.key == before.key || !configDependencyConditionalMacroStateProgress(before, after) || configDependencyConditionalMacroStateProgress(after, before) {
		t.Fatal("recursive source key lost forward counter history")
	}
	if before.counter.next != 0 || after.counter.next != 3 {
		t.Fatal("source keys alias mutable cursors")
	}
	// A consumed value counts even in a short-circuited arithmetic operand:
	// preprocessing expansion precedes integer expression evaluation.
	value, reason := proof.condition("0 && __COUNTER__", state)
	if reason != "" || value != configDependencyMacroUndefined || state.counter.next != 4 {
		t.Fatalf("short-circuit incorrectly skipped expansion: %d %s", value, reason)
	}
}

func TestCompilerCounterParsedCacheStartsEachTranslationUnitFresh(t *testing.T) {
	definitions := map[string]bool{"__COUNTER__": true}
	state, sequence := counterSourceStateForTest(t, "", definitions)
	cache := newConfigDependencyPredefineCache()
	cache.put("initial", "", configDependencyCompilerPredefineParse{state: *state.parent})
	for range 2 {
		cached, ready := cache.get("initial")
		if !ready {
			t.Fatal("initial state not cached")
		}
		current := cached.state.branch()
		if !current.installInitialCompilerCounter(definitions, "exact-compiler-context", "", "measured-definedness", sequence) {
			t.Fatal("fresh TU installation rejected")
		}
		before, valid := configDependencyConditionalMacroStateKey(current)
		if !valid {
			t.Fatal("initial key rejected")
		}
		proof := &configDependencyCallCoverage{}
		if reason := proof.expand("__COUNTER__", current); reason != "" || len(proof.counters) != 1 || proof.counters[0].Token != "7" {
			t.Fatalf("TU inherited another TU's counter: %s %+v", reason, proof.counters)
		}
		after, valid := configDependencyConditionalMacroStateKey(current)
		if !valid || before.key == after.key || !configDependencyConditionalMacroStateProgress(before, after) || before.counter.next != 0 {
			t.Fatal("counter-only progress missing or mutable in recursion key")
		}
		if cached.state.counter != (compilerCounterCursor{}) || cached.state.macroReplacements["__COUNTER__"].counter != nil {
			t.Fatal("TU mutation contaminated cached initial state")
		}
		binding := current.macroReplacements["__COUNTER__"]
		bindingOnly := macroReplacementStateForTest(t, "")
		bindingOnly.macroReplacements["__COUNTER__"] = binding
		if _, accepted := configDependencyPredefineCacheMeasure("binding-only", "", configDependencyCompilerPredefineParse{state: *bindingOnly}, 1<<20, 1<<28); accepted {
			t.Fatal("nontextual binding entered initial parse cache without a cursor")
		}
	}
}

func counterMachineFixture(t *testing.T, sequence *compilerCounterSequence, definitions ...string) (configDependencyMacroCallResolver, configDependencyMacroCallMode) {
	t.Helper()
	builtin := &configDependencyCompilerCounterBinding{context: sequence.context, origin: "test-only-authenticated-binding"}
	builtin.identity = compilerCounterBindingIdentity(builtin.context, builtin.origin)
	bindings := map[string]configDependencyMacroCallBinding{
		"__COUNTER__": {state: configDependencyMacroDefined, counter: builtin},
	}
	for _, text := range definitions {
		d, reason := configDependencyMacroCallDefinitionFromText(text, "test-textual-definition")
		if reason != "" {
			t.Fatal(reason)
		}
		bindings[d.name] = configDependencyMacroCallBinding{state: configDependencyMacroDefined, definition: d}
	}
	return func(name string) (configDependencyMacroCallBinding, string) {
		if binding, ok := bindings[name]; ok {
			return binding, ""
		}
		return configDependencyMacroCallBinding{state: configDependencyMacroUndefined}, ""
	}, configDependencyMacroCallMode{counter: compilerCounterCursor{sequence: sequence, known: true}}
}

func TestCompilerCounterMacroCallTransactions(t *testing.T) {
	sequence, err := parseCompilerCounterSequence("context", 4, "7 42 9 11")
	if err != nil {
		t.Fatal(err)
	}
	resolve, mode := counterMachineFixture(t, sequence, "DROP(x)", "DUP(x) x x", "RAW(x) #x", "STRING(x) RAW(x)", "PASTE(a,b) a ## b", "ALIAS __COUNTER__")
	for _, tc := range []struct {
		text  string
		want  []string
		reads int
	}{
		{"__COUNTER__ __COUNTER__", []string{"7", "42"}, 2},
		{"DROP(__COUNTER__) __COUNTER__", []string{"7"}, 1},
		{"DUP(__COUNTER__) __COUNTER__", []string{"7", "7", "42"}, 2},
		{"RAW(__COUNTER__) STRING(__COUNTER__)", []string{"\"__COUNTER__\"", "\"7\""}, 1},
		{"PASTE(__COUNT,ER__) ALIAS", []string{"7", "42"}, 2},
		{"PASTE(__COUNTER__,tail) __COUNTER__", []string{"__COUNTER__tail", "7"}, 1},
		{"DUP(DUP(__COUNTER__))", []string{"7", "7", "7", "7"}, 1},
	} {
		t.Run(tc.text, func(t *testing.T) {
			got, reason := configDependencyMacroCallExpand(tc.text, resolve, mode)
			if reason != "" || !reflect.DeepEqual(got.Tokens, tc.want) || len(got.CounterReads) != tc.reads || got.Counter.next != tc.reads {
				t.Fatalf("result=%+v reason=%s", got, reason)
			}
			for n, read := range got.CounterReads {
				if read.Ordinal != n || read.SequenceID != sequence.identity || read.Token != sequence.values[n] {
					t.Fatalf("invalid ordered read: %+v", read)
				}
			}
			if mode.counter.next != 0 {
				t.Fatal("caller cursor mutated")
			}
		})
	}
	for _, text := range []string{"__COUNTER__ DUP(", "__COUNTER__ _Pragma(\"x\")", "__COUNTER__ #", strings.Repeat("__COUNTER__ ", 5)} {
		got, reason := configDependencyMacroCallExpand(text, resolve, mode)
		if reason == "" || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) || mode.counter.next != 0 {
			t.Fatalf("failed transaction published partial state: %+v %s", got, reason)
		}
	}
	first, reason := configDependencyMacroCallExpand("__COUNTER__", resolve, mode)
	if reason != "" {
		t.Fatal(reason)
	}
	continued := mode
	continued.counter = first.Counter
	second, reason := configDependencyMacroCallExpand("__COUNTER__", resolve, continued)
	if reason != "" || !reflect.DeepEqual(second.Tokens, []string{"42"}) || first.Counter.next != 1 {
		t.Fatalf("continuation lost or aliased state: %+v %s", second, reason)
	}
	// A textual replacement revokes builtin behavior, even for this spelling.
	redefined, redefinedMode := counterMachineFixture(t, sequence, "__COUNTER__ 73")
	got, reason := configDependencyMacroCallExpand("__COUNTER__ __COUNTER__", redefined, redefinedMode)
	if reason != "" || !reflect.DeepEqual(got.Tokens, []string{"73", "73"}) || got.Counter.next != 0 || len(got.CounterReads) != 0 {
		t.Fatalf("textual replacement acquired counter behavior: %+v %s", got, reason)
	}
}

func TestCompilerCounterMacroCallRejectsConflictingBinding(t *testing.T) {
	sequence, err := parseCompilerCounterSequence("context", 1, "7")
	if err != nil {
		t.Fatal(err)
	}
	resolve, mode := counterMachineFixture(t, sequence)
	binding, _ := resolve("__COUNTER__")
	text, _ := configDependencyMacroCallDefinitionFromText("__COUNTER__ 1", "ordinary")
	for _, variant := range []string{"undefined", "textual", "wrong-name", "bad-identity", "wrong-context", "unknown-cursor"} {
		t.Run(variant, func(t *testing.T) {
			candidate, candidateMode, name := binding, mode, "__COUNTER__"
			switch variant {
			case "undefined":
				candidate.state = configDependencyMacroUndefined
			case "textual":
				candidate.definition = text
			case "wrong-name":
				name = "COUNTER"
			case "bad-identity":
				copy := *candidate.counter
				copy.identity = "bad"
				candidate.counter = &copy
			case "wrong-context":
				copy := *candidate.counter
				copy.context = "different"
				copy.identity = compilerCounterBindingIdentity(copy.context, copy.origin)
				candidate.counter = &copy
			case "unknown-cursor":
				candidateMode.counter = compilerCounterCursor{}
			}
			got, reason := configDependencyMacroCallExpand(name, func(string) (configDependencyMacroCallBinding, string) { return candidate, "" }, candidateMode)
			if reason == "" || !reflect.DeepEqual(got, configDependencyMacroCallResult{}) {
				t.Fatalf("accepted invalid binding/state: %+v", got)
			}
		})
	}
}

func TestCompilerCounterSequenceKeepsMeasuredValues(t *testing.T) {
	source, err := compilerCounterSequenceSource(3)
	if err != nil || source != "__COUNTER__\n__COUNTER__\n__COUNTER__\n" {
		t.Fatalf("%q,%v", source, err)
	}
	sequence, err := parseCompilerCounterSequence("exact-context", 3, "7\n42\n9\n")
	if err != nil {
		t.Fatal(err)
	}
	start := compilerCounterCursor{sequence: sequence, known: true}
	current := start
	for ordinal, want := range []string{"7", "42", "9"} {
		read, next, err := current.expand("exact-context")
		if err != nil || read.Token != want || read.Ordinal != ordinal || read.SequenceID == "" || next.next != ordinal+1 || current.next != ordinal {
			t.Fatalf("read=%+v next=%+v err=%v", read, next, err)
		}
		current = next
	}
	if start.next != 0 {
		t.Fatal("expansion mutated borrowed branch")
	}
	if read, next, err := current.expand("exact-context"); err == nil || read != (compilerCounterRead{}) || next != current {
		t.Fatal("exhaustion consumed or published partial state")
	}
	if read, next, err := start.expand("other-context"); err == nil || read != (compilerCounterRead{}) || next != start {
		t.Fatal("context mismatch consumed state")
	}
}

func TestCompilerCounterSequenceRejectsIncompleteOrUnmodeledAnswers(t *testing.T) {
	for _, input := range []string{"", "0", "0 1 2", "0 __COUNTER__", "0 (1)", "0 -1", "0\u00a01", strings.Repeat(" ", MaxProbeInterpolatedBytes+1)} {
		if sequence, err := parseCompilerCounterSequence("context", 2, input); err == nil || sequence != nil {
			t.Fatalf("accepted invalid sequence of %d bytes", len(input))
		}
	}
	for _, count := range []int{-1, 0, maxCompilerCounterExpansions + 1} {
		if source, err := compilerCounterSequenceSource(count); err == nil || source != "" {
			t.Fatalf("accepted count %d", count)
		}
	}
	if sequence, err := parseCompilerCounterSequence("", 1, "0"); err == nil || sequence != nil {
		t.Fatal("accepted missing context")
	}
}

func TestCompilerCounterCursorJoinTracksPositionNotValue(t *testing.T) {
	sequence, err := parseCompilerCounterSequence("context", 3, "17 17 17")
	if err != nil {
		t.Fatal(err)
	}
	start := compilerCounterCursor{sequence: sequence, known: true}
	_, one, err := start.expand("context")
	if err != nil {
		t.Fatal(err)
	}
	if joined := joinCompilerCounterCursors(start, one); joined.known {
		t.Fatal("equal values hid different expansion histories")
	}
	if joined := joinCompilerCounterCursors(one, one); joined != one {
		t.Fatal("identical histories did not join")
	}
	other, err := parseCompilerCounterSequence("different-context", 3, "17 17 17")
	if err != nil {
		t.Fatal(err)
	}
	if joined := joinCompilerCounterCursors(start, compilerCounterCursor{sequence: other, known: true}); joined.known {
		t.Fatal("compiler contexts were merged")
	}
	if joined := joinCompilerCounterCursors(start, compilerCounterCursor{}); joined.known {
		t.Fatal("unknown history became known")
	}
	for _, position := range []int{-1, len(sequence.values) + 1} {
		invalid := start
		invalid.next = position
		if joined := joinCompilerCounterCursors(invalid, invalid); joined.known {
			t.Fatal("identical invalid positions became known")
		}
	}
	exhausted := start
	exhausted.next = len(sequence.values)
	if joined := joinCompilerCounterCursors(exhausted, exhausted); joined != exhausted {
		t.Fatal("valid exhausted history failed to join")
	}
}

func TestCompilerCounterSourceStateBranchTransactions(t *testing.T) {
	sequence, err := parseCompilerCounterSequence("context", 4, "17 17 17 17")
	if err != nil {
		t.Fatal(err)
	}
	resolve, mode := counterMachineFixture(t, sequence)
	rootState := func() *configDependencyMacroState {
		state := macroReplacementStateForTest(t, "")
		state.counter = mode.counter // Test-only installation, not a source binding.
		return state
	}
	advance := func(state *configDependencyMacroState) configDependencyMacroCallResult {
		t.Helper()
		current := mode
		current.counter = state.counter
		result, reason := configDependencyMacroCallExpand("__COUNTER__", resolve, current)
		if reason != "" || !state.commitCounterExpansion(current.counter, result) {
			t.Fatalf("source transition rejected: %s", reason)
		}
		return result
	}
	t.Run("nested include commit", func(t *testing.T) {
		root := rootState()
		child, sibling := root.branch(), root.branch()
		grandchild := child.branch()
		advance(grandchild)
		if root.counter.next != 0 || child.counter.next != 0 || sibling.counter.next != 0 {
			t.Fatal("speculative expansion leaked into another branch")
		}
		if !child.commitBranch(grandchild) || child.counter.next != 1 || !grandchild.consumed || grandchild.counter != (compilerCounterCursor{}) {
			t.Fatal("inner commit lost state or retained consumed state")
		}
		advance(child)
		before := root.snapshotRevision
		if !root.commitBranch(child) || root.counter.next != 2 || root.snapshotRevision == before || sibling.validSnapshotLineage() {
			t.Fatal("counter-only include effect lost or failed to invalidate sibling")
		}
		preserved := root.counter
		if root.commitBranch(sibling) || root.counter != preserved {
			t.Fatal("stale sibling rewound counter")
		}
	})
	t.Run("branch joins", func(t *testing.T) {
		for _, same := range []bool{false, true} {
			root := rootState()
			left, right := root.branch(), root.branch()
			advance(left)
			if same {
				advance(right)
			}
			root.mergePossibleBranches(left, right)
			if root.tainted || root.counter.known != same || same && root.counter.next != 1 || !left.consumed || !right.consumed {
				t.Fatalf("incorrect counter join, same=%v state=%+v", same, root.counter)
			}
		}
		root := rootState()
		child, sibling := root.branch(), root.branch()
		advance(child)
		root.mergePossibleBranch(child)
		if root.counter.known || sibling.validSnapshotLineage() {
			t.Fatal("possibly executed expansion retained exact state or stale sibling")
		}
	})
	t.Run("failed observation never commits", func(t *testing.T) {
		root := rootState()
		advance(root)
		before, revision := root.counter, root.snapshotRevision
		current := mode
		current.counter = before
		failed, reason := configDependencyMacroCallExpand("__COUNTER__ _Pragma(\"x\")", resolve, current)
		if reason == "" || root.commitCounterExpansion(before, failed) || root.counter != before || root.snapshotRevision != revision {
			t.Fatal("failed expansion published a state prefix")
		}
		good, reason := configDependencyMacroCallExpand("__COUNTER__", resolve, current)
		if reason != "" {
			t.Fatal(reason)
		}
		for _, variant := range []string{"ordinal", "sequence", "token", "missing-read", "cursor"} {
			bad := good
			bad.CounterReads = append([]compilerCounterRead(nil), good.CounterReads...)
			switch variant {
			case "ordinal":
				bad.CounterReads[0].Ordinal++
			case "sequence":
				bad.CounterReads[0].SequenceID = "foreign"
			case "token":
				bad.CounterReads[0].Token = "99"
			case "missing-read":
				bad.CounterReads = nil
			case "cursor":
				bad.Counter.next++
			}
			if root.commitCounterExpansion(before, bad) || root.counter != before || root.snapshotRevision != revision {
				t.Fatalf("invalid %s transition changed source state", variant)
			}
		}
		if !root.commitCounterExpansion(before, good) || root.commitCounterExpansion(before, good) {
			t.Fatal("exact transaction did not commit once")
		}
	})
}
