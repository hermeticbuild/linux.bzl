package kconfig

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCompilerCheckpointPreservesOmittedEmptyRequestFields(t *testing.T) {
	original := kbuildCompilerCheckpoint{Plan: &ProbePlan{Requests: map[string]ProbeRequest{
		"request": {Sources: []string{}, Steps: []ProbeStep{{
			Arguments: []string{}, Environment: map[string]string{},
			Candidate: &ProbeCandidateArguments{Base: []int{}, TranslationUnits: []string{}},
		}}},
	}}}
	var err error
	original.EmptyContainers, err = compilerCheckpointEmptyContainers(original)
	if err != nil || len(original.EmptyContainers) != 5 {
		t.Fatalf("shape = %#v, %v", original.EmptyContainers, err)
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	decode := func() kbuildCompilerCheckpoint {
		var r kbuildCompilerCheckpoint
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := decode()
	if reflect.DeepEqual(r, original) {
		t.Fatal("fixture did not lose empty request fields in protocol JSON")
	}
	if err := restoreCompilerCheckpointEmptyContainers(&r); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, original) {
		t.Fatal("shape transport changed exact request state")
	}
	for _, mutation := range []string{"duplicate", "reordered", "unknown-field", "unknown-key", "scalar", "self", "overdeep", "bad-index", "redundant"} {
		t.Run(mutation, func(t *testing.T) {
			r := decode()
			switch mutation {
			case "duplicate":
				r.EmptyContainers = append(r.EmptyContainers, slices.Clone(r.EmptyContainers[0]))
			case "reordered":
				slices.Reverse(r.EmptyContainers)
			case "unknown-field":
				r.EmptyContainers[0] = []string{"Unknown"}
			case "unknown-key":
				r.EmptyContainers[0] = []string{"Plan", "Requests", "missing"}
			case "scalar":
				r.EmptyContainers[0] = []string{"Schema"}
			case "self":
				r.EmptyContainers[0] = []string{"EmptyContainers"}
			case "overdeep":
				r.EmptyContainers[0] = strings.Split(strings.Repeat("x.", 65), ".")
			case "bad-index":
				r.EmptyContainers[0] = []string{"Plan", "Requests", "request", "Steps", "00", "Arguments"}
			case "redundant":
				r.EmptyContainers[0] = []string{"Definitions"}
			}
			if err := restoreCompilerCheckpointEmptyContainers(&r); err == nil {
				t.Fatal("accepted malformed or noncanonical shape")
			}
		})
	}
}
