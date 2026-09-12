package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"unicode/utf8"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

// Contexts are exact invocation descriptors, not executable requests or source
// receipts. Kind is deliberately absent so both query kinds can share bytes.
// Their slices and maps are immutable after publication and may be borrowed by
// expanded per-variant query views; each membership still needs fresh replay.
type familyCompilerGuardContext struct {
	Scope, Role, Language       string
	Arguments, TranslationUnits []string
	Environment                 map[string]string
}

// encoding/json replaces invalid UTF-8 instead of returning an error. Reject
// those descriptors before hashing so different original invocation bytes can
// never silently collapse to the same replacement-character context.
func (c familyCompilerGuardContext) validate() error {
	for _, value := range []string{c.Scope, c.Role, c.Language} {
		if !utf8.ValidString(value) {
			return fmt.Errorf("compiler guard context has invalid UTF-8 in a scope, role or language")
		}
	}
	for _, value := range c.Arguments {
		if !utf8.ValidString(value) {
			return fmt.Errorf("compiler guard context has an invalid UTF-8 argument")
		}
	}
	for _, value := range c.TranslationUnits {
		if !utf8.ValidString(value) {
			return fmt.Errorf("compiler guard context has an invalid UTF-8 translation unit")
		}
	}
	for name, value := range c.Environment {
		if !utf8.ValidString(name) || !utf8.ValidString(value) {
			return fmt.Errorf("compiler guard context has invalid UTF-8 in its environment")
		}
	}
	return nil
}

func (q familyCompilerGuardQuery) compilerContext() familyCompilerGuardContext {
	return familyCompilerGuardContext{
		Scope: q.Scope, Role: q.Role, Language: q.Language,
		Arguments: q.Arguments, TranslationUnits: q.TranslationUnits, Environment: q.Environment,
	}
}

func (c familyCompilerGuardContext) id() string {
	data, _ := json.Marshal(c)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (c familyCompilerGuardContext) bind(q familyCompilerGuardQuery) familyCompilerGuardQuery {
	q.Scope, q.Role, q.Language = c.Scope, c.Role, c.Language
	q.Arguments, q.TranslationUnits, q.Environment = c.Arguments, c.TranslationUnits, c.Environment
	return q
}

func equalFamilyCompilerGuardContexts(left, right familyCompilerGuardContext) bool {
	return left.Scope == right.Scope && left.Role == right.Role && left.Language == right.Language &&
		(left.Arguments == nil) == (right.Arguments == nil) && slices.Equal(left.Arguments, right.Arguments) &&
		(left.TranslationUnits == nil) == (right.TranslationUnits == nil) && slices.Equal(left.TranslationUnits, right.TranslationUnits) &&
		(left.Environment == nil) == (right.Environment == nil) && maps.Equal(left.Environment, right.Environment)
}

type familyCompilerGuardWireQuery struct {
	Context string
	Kind    string `json:",omitempty"`
	Names   []string
	Calls   []kconfig.CompilerIntrinsicCall `json:",omitempty"`
}

type familyCompilerGuardWireManifest struct {
	Schema    string
	Round     int
	Previous  []string
	Toolsets  map[string]string
	Strings   []string
	Contexts  map[string]familyCompilerGuardPackedContext
	Variants  map[string][]familyCompilerGuardWireQuery
	PlanID    string
	Truncated bool
}

// Keep payload validation in the coordinator: callers also use this serializer
// to create negative fixtures. It does not add a newline or mutate input views.
func marshalFamilyCompilerGuardManifest(m familyCompilerGuardManifest) ([]byte, error) {
	wire := familyCompilerGuardWireManifest{
		Schema: m.Schema, Round: m.Round, Previous: m.Previous, Toolsets: m.Toolsets,
		PlanID: m.PlanID, Truncated: m.Truncated,
	}
	contexts := map[string]familyCompilerGuardContext{}
	if m.Variants != nil {
		wire.Variants = make(map[string][]familyCompilerGuardWireQuery, len(m.Variants))
	}
	for name, queries := range m.Variants {
		var entries []familyCompilerGuardWireQuery
		if queries != nil {
			entries = make([]familyCompilerGuardWireQuery, len(queries))
		}
		for index, query := range queries {
			context := query.compilerContext()
			if err := context.validate(); err != nil {
				return nil, err
			}
			id := context.id()
			if previous, found := contexts[id]; found {
				if !equalFamilyCompilerGuardContexts(previous, context) {
					return nil, fmt.Errorf("compiler guard context ID has contradictory contents")
				}
			} else {
				// These are temporary read-only views used by synchronous JSON
				// encoding, not retained copies of the caller's compiler scopes.
				contexts[id] = context
			}
			entries[index] = familyCompilerGuardWireQuery{
				Context: id, Kind: query.Kind, Names: query.Names, Calls: query.Calls,
			}
		}
		wire.Variants[name] = entries
	}
	wire.Strings, wire.Contexts = packFamilyCompilerGuardContexts(contexts)
	return json.Marshal(wire)
}

func validFamilyCompilerGuardContextID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for index := range id {
		if value := id[index]; !(value >= '0' && value <= '9' || value >= 'a' && value <= 'f') {
			return false
		}
	}
	return true
}

// The caller first performs raw structural preflight, then compares a remarshal
// against the original bytes. Those gates reject duplicate/aliased JSON fields
// before discarded decoder values can evade allocation or canonicality checks.
// This function validates the compact graph before expanding shared query views.
func unmarshalFamilyCompilerGuardManifest(data []byte) (*familyCompilerGuardManifest, error) {
	if len(data) > maxFamilyCompilerGuardBytes {
		return nil, fmt.Errorf("compiler guard manifest exceeds its byte budget")
	}
	var wire familyCompilerGuardWireManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("compiler guard manifest has trailing content")
	}
	if wire.Contexts == nil {
		return nil, fmt.Errorf("compiler guard manifest requires a context table")
	}
	if len(wire.Contexts) > maxFamilyCompilerGuardQueries {
		return nil, fmt.Errorf("compiler guard context table exceeds its budget")
	}
	contexts, err := unpackFamilyCompilerGuardContexts(wire.Strings, wire.Contexts)
	if err != nil {
		return nil, err
	}
	memberships := 0
	used := make(map[string]bool, len(wire.Contexts))
	for _, entries := range wire.Variants {
		memberships += len(entries)
		if memberships > maxFamilyCompilerGuardMemberships {
			return nil, fmt.Errorf("compiler guard manifest exceeds its membership budget")
		}
		for _, entry := range entries {
			if !validFamilyCompilerGuardContextID(entry.Context) {
				return nil, fmt.Errorf("compiler guard query has an invalid context reference")
			}
			if _, found := wire.Contexts[entry.Context]; !found {
				return nil, fmt.Errorf("compiler guard query references a missing context")
			}
			used[entry.Context] = true
		}
	}
	if len(used) != len(wire.Contexts) {
		return nil, fmt.Errorf("compiler guard manifest contains an unused context")
	}
	m := &familyCompilerGuardManifest{
		Schema: wire.Schema, Round: wire.Round, Previous: wire.Previous, Toolsets: wire.Toolsets,
		PlanID: wire.PlanID, Truncated: wire.Truncated,
	}
	if wire.Variants != nil {
		m.Variants = make(map[string][]familyCompilerGuardQuery, len(wire.Variants))
	}
	for name, entries := range wire.Variants {
		var queries []familyCompilerGuardQuery
		if entries != nil {
			queries = make([]familyCompilerGuardQuery, len(entries))
		}
		for index, entry := range entries {
			queries[index] = contexts[entry.Context].bind(familyCompilerGuardQuery{
				Kind: entry.Kind, Names: entry.Names, Calls: entry.Calls,
			})
		}
		m.Variants[name] = queries
	}
	return m, nil
}
