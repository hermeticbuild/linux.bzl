package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"
)

// This is lossless wire interning, not compiler-context equivalence. IDs still
// hash the full original descriptors, and every query is independently replayed.
// Limits are fixed independently of the number of variants.
const maxFamilyCompilerGuardStrings = 1 << 20
const maxFamilyCompilerGuardReferences = 1 << 22

type familyCompilerGuardPackedContext struct {
	Scope, Role, Language       string
	Arguments, TranslationUnits []int
	Environment                 [][2]int
}

func packFamilyCompilerGuardContext(c familyCompilerGuardContext, index func(string) int) familyCompilerGuardPackedContext {
	vector := func(values []string) []int {
		if values == nil {
			return nil
		}
		result := make([]int, len(values))
		for i, value := range values {
			result[i] = index(value)
		}
		return result
	}
	p := familyCompilerGuardPackedContext{
		Scope: c.Scope, Role: c.Role, Language: c.Language,
		Arguments: vector(c.Arguments), TranslationUnits: vector(c.TranslationUnits),
	}
	if c.Environment != nil {
		p.Environment = make([][2]int, 0, len(c.Environment))
		for _, name := range slices.Sorted(maps.Keys(c.Environment)) {
			p.Environment = append(p.Environment, [2]int{index(name), index(c.Environment[name])})
		}
	}
	return p
}

// Preflight uses a seven-digit reference even for a one-entry dictionary, so
// adding lexically earlier strings cannot invalidate previously charged bytes.
// It returns only new strings and never mutates admission state. Optional queries
// commit this delta only after all membership, payload and expansion checks pass.
func familyCompilerGuardContextCost(c familyCompilerGuardContext, words map[string]struct{}) (int, map[string]struct{}, int) {
	added := map[string]struct{}{}
	references, cost := 0, 0
	packed := packFamilyCompilerGuardContext(c, func(value string) int {
		references++
		if _, found := words[value]; !found {
			if _, found := added[value]; !found {
				encoded, _ := json.Marshal(value)
				cost += len(encoded) + 1
				added[value] = struct{}{}
			}
		}
		return maxFamilyCompilerGuardStrings - 1
	})
	encoded, _ := json.Marshal(packed)
	cost += len(encoded) + 64 + 4
	if words == nil {
		cost += 32 // String-table field and array framing, charged once.
	}
	return cost, added, references
}

func packFamilyCompilerGuardContexts(contexts map[string]familyCompilerGuardContext) ([]string, map[string]familyCompilerGuardPackedContext) {
	words := map[string]struct{}{}
	for _, c := range contexts {
		for _, values := range [][]string{c.Arguments, c.TranslationUnits} {
			for _, value := range values {
				words[value] = struct{}{}
			}
		}
		for name, value := range c.Environment {
			words[name], words[value] = struct{}{}, struct{}{}
		}
	}
	strings := slices.Sorted(maps.Keys(words))
	indices := make(map[string]int, len(strings))
	for i, value := range strings {
		indices[value] = i
	}
	packed := make(map[string]familyCompilerGuardPackedContext, len(contexts))
	for id, c := range contexts {
		packed[id] = packFamilyCompilerGuardContext(c, func(value string) int { return indices[value] })
	}
	return strings, packed
}

// Check expansion BEFORE allocating restored vectors/maps or hashing full
// descriptors. A small repeated reference must not amplify a large word beyond
// the original expanded-work ceiling. Membership expansion is checked again by
// the coordinator; this gate bounds the unique descriptor table itself.
func unpackFamilyCompilerGuardContexts(words []string, packed map[string]familyCompilerGuardPackedContext) (map[string]familyCompilerGuardContext, error) {
	if packed == nil || len(packed) > maxFamilyCompilerGuardQueries || len(words) > maxFamilyCompilerGuardStrings {
		return nil, fmt.Errorf("compiler guard dictionary exceeds its table budget")
	}
	costs := make([]int, len(words))
	for i, value := range words {
		if !utf8.ValidString(value) || i > 0 && words[i-1] >= value {
			return nil, fmt.Errorf("compiler guard string dictionary is not canonical")
		}
		encoded, _ := json.Marshal(value)
		costs[i] = len(encoded) + 1
	}
	used := make([]bool, len(words))
	references, expanded := 0, 0
	check := func(index int) error {
		if index < 0 || index >= len(words) {
			return fmt.Errorf("compiler guard context references a missing string")
		}
		references++
		expanded += costs[index]
		if references > maxFamilyCompilerGuardReferences || expanded > maxFamilyCompilerGuardExpandedBytes {
			return fmt.Errorf("compiler guard dictionary exceeds its expanded budget")
		}
		used[index] = true
		return nil
	}
	for id, c := range packed {
		if !validFamilyCompilerGuardContextID(id) {
			return nil, fmt.Errorf("compiler guard context table has an invalid content ID")
		}
		// Marshal only the non-interned fields here: JSON escaping can cost
		// more than raw UTF-8 lengths. The constant bounds remaining framing.
		small, _ := json.Marshal([3]string{c.Scope, c.Role, c.Language})
		expanded += len(small) + 256
		if expanded > maxFamilyCompilerGuardExpandedBytes {
			return nil, fmt.Errorf("compiler guard dictionary exceeds its expanded budget")
		}
		for _, values := range [][]int{c.Arguments, c.TranslationUnits} {
			for _, index := range values {
				if err := check(index); err != nil {
					return nil, err
				}
			}
		}
		previous := -1
		for _, pair := range c.Environment {
			if pair[0] <= previous {
				return nil, fmt.Errorf("compiler guard dictionary environment is not canonical")
			}
			previous = pair[0]
			for _, index := range pair {
				if err := check(index); err != nil {
					return nil, err
				}
			}
		}
	}
	if slices.Contains(used, false) {
		return nil, fmt.Errorf("compiler guard dictionary contains an unused string")
	}
	vector := func(indices []int) []string {
		if indices == nil {
			return nil
		}
		result := make([]string, len(indices))
		for i, index := range indices {
			result[i] = words[index]
		}
		return result
	}
	contexts := make(map[string]familyCompilerGuardContext, len(packed))
	for id, p := range packed {
		c := familyCompilerGuardContext{
			Scope: p.Scope, Role: p.Role, Language: p.Language,
			Arguments: vector(p.Arguments), TranslationUnits: vector(p.TranslationUnits),
		}
		if p.Environment != nil {
			c.Environment = make(map[string]string, len(p.Environment))
			for _, pair := range p.Environment {
				c.Environment[words[pair[0]]] = words[pair[1]]
			}
		}
		if err := c.validate(); err != nil {
			return nil, err
		}
		if c.id() != id {
			return nil, fmt.Errorf("compiler guard context table has an invalid content ID")
		}
		contexts[id] = c
	}
	return contexts, nil
}
