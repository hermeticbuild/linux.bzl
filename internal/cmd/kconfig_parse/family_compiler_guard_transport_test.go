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

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func familyCompilerGuardTransportManifestForTest() familyCompilerGuardManifest {
	query := familyCompilerGuardQuery{
		Scope: "target", Role: "cc", Language: "c",
		Arguments: []string{"-nostdinc", "-DFEATURE=1", "-UFEATURE"}, TranslationUnits: []string{"fixture.c"},
		Environment: map[string]string{"LANG": "C", "EXACT_VALUE": "one two"}, Names: []string{"__FEATURE"},
	}
	return familyCompilerGuardManifest{
		Schema: familyCompilerGuardSchema, Toolsets: map[string]string{"target": "fixture-identity"},
		Variants: map[string][]familyCompilerGuardQuery{"base": {query}}, PlanID: "fixture-plan",
	}
}

func familyCompilerGuardTransportBytesForTest(t *testing.T, manifest familyCompilerGuardManifest) []byte {
	t.Helper()
	data, err := marshalFamilyCompilerGuardManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] == '\n' {
		t.Fatal("transport serializer did not return JSON without a trailing newline")
	}
	return data
}

func TestFamilyCompilerGuardTransportCompactsKindsAndVariants(t *testing.T) {
	m := familyCompilerGuardTransportManifestForTest()
	defined := m.Variants["base"][0]
	intrinsic := defined
	intrinsic.Kind, intrinsic.Names = familyCompilerIntrinsicQueryKind, nil
	intrinsic.Calls = []kconfig.CompilerIntrinsicCall{
		{Operator: "__has_attribute", Operand: "deprecated"},
		{Operator: "__has_builtin", Operand: "deprecated"},
	}
	m.Variants["base"] = []familyCompilerGuardQuery{defined, intrinsic}
	m.Variants["other"] = []familyCompilerGuardQuery{intrinsic, defined}
	m.Variants["nil"] = nil
	m.Variants["empty"] = []familyCompilerGuardQuery{}
	data := familyCompilerGuardTransportBytesForTest(t, m)
	var wire familyCompilerGuardWireManifest
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Contexts) != 1 || bytes.Count(data, []byte(`"Arguments"`)) != 1 {
		t.Fatal("identical context bytes were repeated across kinds or variants")
	}
	for _, queries := range wire.Variants {
		for _, query := range queries {
			if query.Context != defined.compilerContext().id() {
				t.Fatal("query kind or variant changed the shared context identity")
			}
		}
	}
	got, err := unmarshalFamilyCompilerGuardManifest(data)
	if err != nil || !reflect.DeepEqual(got, &m) {
		t.Fatalf("compact transport did not preserve expanded query views: %v", err)
	}
	base, other := got.Variants["base"][0], got.Variants["other"][0]
	if &base.Arguments[0] != &other.Arguments[0] || &base.TranslationUnits[0] != &other.TranslationUnits[0] {
		t.Fatal("decoder cloned context slices per membership")
	}
	// Deliberately mutate only this disposable fixture to verify borrowed map
	// storage; production treats these views as immutable. Restore immediately.
	base.Environment["FIXTURE_ONLY"] = "shared"
	if other.Environment["FIXTURE_ONLY"] != "shared" || defined.Environment["FIXTURE_ONLY"] != "" {
		t.Fatal("decoded context maps are not shared, or retained caller storage")
	}
	delete(base.Environment, "FIXTURE_ONLY")
	if !bytes.Equal(data, familyCompilerGuardTransportBytesForTest(t, *got)) {
		t.Fatal("compact round trip changed canonical bytes")
	}
}

func TestFamilyCompilerGuardTransportPreservesCompilerRequestPlan(t *testing.T) {
	m := familyCompilerGuardTransportManifestForTest()
	m.Toolsets = familyCompilerGuardPlanForTest(t).Toolsets
	intrinsic := m.Variants["base"][0]
	intrinsic.Kind, intrinsic.Names = familyCompilerIntrinsicQueryKind, nil
	intrinsic.Calls = []kconfig.CompilerIntrinsicCall{
		{Operator: "__has_attribute", Operand: "deprecated"},
		{Operator: "__has_attribute", Operand: "nodiscard"},
		{Operator: "__has_builtin", Operand: "deprecated"},
		{Operator: "__has_builtin", Operand: "nodiscard"},
	}
	m.Variants["base"] = append(m.Variants["base"], intrinsic)
	m.Variants["other"] = slices.Clone(m.Variants["base"])
	decoded, err := unmarshalFamilyCompilerGuardManifest(familyCompilerGuardTransportBytesForTest(t, m))
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"base", "other"} {
		var want string
		for index, queries := range [][]familyCompilerGuardQuery{m.Variants[variant], decoded.Variants[variant]} {
			// Each membership list is independently rebound to a fresh ordinary
			// registry/toolset context, not to a cached batch from another variant.
			scopes := familyCompilerGuardCarryScopesForTest(t, m.Toolsets)
			batch, err := kconfig.NewKbuildCompilerGuardBatch(scopes, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, query := range queries {
				if ready, err := query.evaluate(batch); err != nil || ready {
					t.Fatalf("fresh discovery unexpectedly answered or rejected a query: %t/%v", ready, err)
				}
			}
			plan, err := batch.Plan()
			if err != nil {
				t.Fatal(err)
			}
			id, err := familyCompilerGuardPlanID(plan)
			if err != nil || len(plan.Terminal) != 2 {
				t.Fatalf("both query kinds did not produce a complete plan: %v", err)
			}
			if index == 0 {
				want = id
			} else if id != want {
				t.Fatal("compact transport changed canonical compiler requests, node IDs or dependency bindings")
			}
		}
	}
}

func TestFamilyCompilerGuardTransportContextIdentityIsExact(t *testing.T) {
	base := familyCompilerGuardTransportManifestForTest().Variants["base"][0].compilerContext()
	for _, field := range []string{"scope", "role", "language", "argument order", "argument value", "translation units", "environment"} {
		t.Run(field, func(t *testing.T) {
			changed := base
			changed.Arguments, changed.TranslationUnits, changed.Environment = slices.Clone(base.Arguments), slices.Clone(base.TranslationUnits), maps.Clone(base.Environment)
			switch field {
			case "scope":
				changed.Scope = "host"
			case "role":
				changed.Role = "cxx"
			case "language":
				changed.Language = "c++"
			case "argument order":
				slices.Reverse(changed.Arguments)
			case "argument value":
				changed.Arguments[1] = "-DFEATURE=2"
			case "translation units":
				changed.TranslationUnits[0] = "other.c"
			case "environment":
				changed.Environment["EXACT_VALUE"] = "one  two"
			}
			if changed.id() == base.id() || equalFamilyCompilerGuardContexts(base, changed) {
				t.Fatal("different original invocation descriptors were conflated")
			}
			m := familyCompilerGuardTransportManifestForTest()
			m.Variants["other"] = []familyCompilerGuardQuery{changed.bind(m.Variants["base"][0])}
			got, err := unmarshalFamilyCompilerGuardManifest(familyCompilerGuardTransportBytesForTest(t, m))
			if err != nil || !reflect.DeepEqual(got, &m) {
				t.Fatalf("exact field change did not round trip: %v", err)
			}
		})
	}
	contexts := []familyCompilerGuardContext{
		{}, {Arguments: []string{}}, {TranslationUnits: []string{}}, {Environment: map[string]string{}},
		{Arguments: []string{}, TranslationUnits: []string{}, Environment: map[string]string{}},
	}
	m := familyCompilerGuardTransportManifestForTest()
	m.Variants = map[string][]familyCompilerGuardQuery{}
	ids := map[string]bool{}
	for index, context := range contexts {
		id := context.id()
		if ids[id] || !validFamilyCompilerGuardContextID(id) {
			t.Fatal("nil and empty context fields share an identity")
		}
		ids[id] = true
		m.Variants[fmt.Sprint(index)] = []familyCompilerGuardQuery{context.bind(familyCompilerGuardQuery{Names: []string{"__A"}})}
	}
	got, err := unmarshalFamilyCompilerGuardManifest(familyCompilerGuardTransportBytesForTest(t, m))
	if err != nil || !reflect.DeepEqual(got, &m) {
		t.Fatalf("nil-versus-empty fields were normalized: %v", err)
	}
}

func TestFamilyCompilerGuardTransportRejectsInvalidUTF8(t *testing.T) {
	for _, field := range []string{"scope", "role", "language", "argument", "translation unit", "environment key", "environment value"} {
		for _, invalid := range []string{"\xff", "\xfe"} {
			t.Run(fmt.Sprintf("%s/%x", field, invalid), func(t *testing.T) {
				m := familyCompilerGuardTransportManifestForTest()
				context := m.Variants["base"][0].compilerContext()
				value := "SENSITIVE" + invalid
				switch field {
				case "scope":
					context.Scope = value
				case "role":
					context.Role = value
				case "language":
					context.Language = value
				case "argument":
					context.Arguments = []string{value}
				case "translation unit":
					context.TranslationUnits = []string{value}
				case "environment key":
					context.Environment = map[string]string{value: "ordinary"}
				case "environment value":
					context.Environment = map[string]string{"ordinary": value}
				}
				if err := context.validate(); err == nil || strings.Contains(err.Error(), "SENSITIVE") {
					t.Fatal("invalid UTF-8 was accepted or leaked through diagnostics")
				}
				m.Variants["base"][0] = context.bind(m.Variants["base"][0])
				if data, err := marshalFamilyCompilerGuardManifest(m); err == nil || data != nil || strings.Contains(err.Error(), "SENSITIVE") {
					t.Fatal("single malformed context was silently JSON-normalized")
				}
			})
		}
	}
	valid := familyCompilerGuardContext{Arguments: []string{"\ufffd"}}
	if err := valid.validate(); err != nil {
		t.Fatal("a genuinely valid replacement-character string was rejected")
	}
}

func TestFamilyCompilerGuardTransportRejectsInvalidContextGraph(t *testing.T) {
	for _, change := range []string{"missing table", "empty table", "bad table ID", "changed table contents", "missing reference", "bad reference", "dangling reference", "unused context", "too many contexts", "too many memberships"} {
		t.Run(change, func(t *testing.T) {
			var wire familyCompilerGuardWireManifest
			if err := json.Unmarshal(familyCompilerGuardTransportBytesForTest(t, familyCompilerGuardTransportManifestForTest()), &wire); err != nil {
				t.Fatal(err)
			}
			id := wire.Variants["base"][0].Context
			context := wire.Contexts[id]
			switch change {
			case "missing table":
				wire.Contexts = nil
			case "empty table":
				wire.Contexts = map[string]familyCompilerGuardPackedContext{}
			case "bad table ID":
				delete(wire.Contexts, id)
				wire.Contexts[strings.ToUpper(id)] = context
			case "changed table contents":
				context.Scope = "changed"
				wire.Contexts[id] = context
			case "missing reference":
				wire.Variants["base"][0].Context = ""
			case "bad reference":
				wire.Variants["base"][0].Context = "../" + id
			case "dangling reference":
				wire.Variants["base"][0].Context = strings.Repeat("0", 64)
			case "unused context":
				context.Scope = "host"
				original := familyCompilerGuardTransportManifestForTest().Variants["base"][0].compilerContext()
				original.Scope = "host"
				wire.Contexts[original.id()] = context
			case "too many contexts":
				for index := range maxFamilyCompilerGuardQueries + 1 {
					wire.Contexts[fmt.Sprintf("%064x", index)] = context
				}
			case "too many memberships":
				query := wire.Variants["base"][0]
				wire.Variants["base"] = make([]familyCompilerGuardWireQuery, maxFamilyCompilerGuardMemberships+1)
				for index := range wire.Variants["base"] {
					wire.Variants["base"][index] = query
				}
			}
			data, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := unmarshalFamilyCompilerGuardManifest(data); err == nil || got != nil {
				t.Fatal("invalid compact context graph was accepted")
			}
		})
	}
}

func TestFamilyCompilerGuardTransportStrictDecodeAndCanonicalBoundary(t *testing.T) {
	data := familyCompilerGuardTransportBytesForTest(t, familyCompilerGuardTransportManifestForTest())
	for _, malformed := range [][]byte{
		append(slices.Clone(data), []byte("{}")...),
		[]byte("null"),
		bytes.Replace(data, []byte(`"Round":`), []byte(`"Unexpected":1,"Round":`), 1),
		bytes.Replace(data, []byte(`"Scope":`), []byte(`"Unexpected":1,"Scope":`), 1),
		bytes.Replace(data, []byte(`"Context":`), []byte(`"Arguments":[],"Context":`), 1),
	} {
		if got, err := unmarshalFamilyCompilerGuardManifest(malformed); err == nil || got != nil {
			t.Fatal("unknown fields or trailing data were accepted")
		}
	}
	// The external preflight counts raw duplicates, and canonical remarshal is
	// deliberately the gate for aliases/duplicate fields that JSON can decode.
	for _, malformed := range [][]byte{
		bytes.Replace(data, []byte(`"Contexts":`), []byte(`"cOnTeXtS":`), 1),
		bytes.Replace(data, []byte(`"Contexts":`), []byte(`"\u0043ontexts":`), 1),
		bytes.Replace(data, []byte(`{`), []byte(`{"Contexts":{},`), 1),
		bytes.Replace(data, []byte(`{`), []byte(`{"Variants":{},`), 1),
	} {
		got, err := unmarshalFamilyCompilerGuardManifest(malformed)
		if err == nil && bytes.Equal(malformed, familyCompilerGuardTransportBytesForTest(t, *got)) {
			t.Fatal("duplicate or aliased fields bypassed canonical remarshal")
		}
	}
}

func TestFamilyCompilerGuardTransportLeavesPayloadValidationToCoordinator(t *testing.T) {
	m := familyCompilerGuardTransportManifestForTest()
	query := m.Variants["base"][0]
	query.Kind = familyCompilerIntrinsicQueryKind
	query.Calls = []kconfig.CompilerIntrinsicCall{{Operator: "unmodeled", Operand: "bad operand"}}
	// Mixed Names/Calls must remain serializable for existing negative fixtures.
	m.Variants["base"][0] = query
	got, err := unmarshalFamilyCompilerGuardManifest(familyCompilerGuardTransportBytesForTest(t, m))
	if err != nil || !reflect.DeepEqual(got, &m) || got.Variants["base"][0].validatePayload() == nil {
		t.Fatalf("transport prematurely validated or altered a negative fixture: %v", err)
	}
	for _, variants := range []map[string][]familyCompilerGuardQuery{nil, {}, {"base": nil}, {"base": {}}} {
		m.Variants = variants
		got, err := unmarshalFamilyCompilerGuardManifest(familyCompilerGuardTransportBytesForTest(t, m))
		if err != nil || !reflect.DeepEqual(got, &m) {
			t.Fatalf("empty transport did not preserve variant shape: %v", err)
		}
	}
}
