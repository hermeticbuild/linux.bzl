package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

type variadicCommaCase struct {
	name, syntax, standard string
	rejected               bool
}

func variadicCommaCases() []variadicCommaCase {
	return []variadicCommaCase{
		{"standard-c11", "standard", "c11", false},
		{"standard-gnu11", "standard", "gnu11", false},
		{"named-c11", "named", "c11", false},
		{"named-gnu11", "named", "gnu11", false},
		{"standard-conflicting-defines", "standard", "gnu11", true},
		{"named-conflicting-defines", "named", "gnu11", true},
	}
}

func (test variadicCommaCase) arguments() []string {
	args := []string{"-nostdinc", "-std=" + test.standard, "-Werror", "-DUNRELATED=73"}
	if test.rejected {
		// These unused conflicting writes must remain observable diagnostics.
		args = append(args, "-DUNRELATED=74")
	}
	return args
}

type variadicCommaReferences struct{ managed, direct kconfig.ProbeReference }
type variadicCommaResult struct {
	NodeID       string `json:"node_id"`
	DirectNodeID string `json:"direct_node_id"`
	DeleteComma  *bool  `json:"delete_comma"`
	ExitCode     int    `json:"exit_code"`
}

func (f *fixture) discoverVariadicCommas() (*kconfig.ProbePlan, error) {
	batch, err := kconfig.NewKbuildCompilerGuardBatch(f.scopes, nil)
	if err != nil {
		return nil, err
	}
	builder, err := kconfig.NewProbePlanBuilder(f.toolset, "")
	if err != nil {
		return nil, err
	}
	f.variadicCommas = map[string]variadicCommaReferences{}
	var directTerminals []kconfig.ProbeReference
	for _, test := range variadicCommaCases() {
		deleted, state, managed, err := batch.OptionalCompilerVariadicCommaAttempt("target", "cc", "c", test.arguments(), nil, test.syntax, nil)
		if err != nil {
			return nil, err
		}
		if deleted || state != kconfig.OptionalCompilerVariadicCommaPending {
			return nil, fmt.Errorf("variadic discovery fabricated an answer")
		}
		formal, tail := "...", "__VA_ARGS__"
		if test.syntax == "named" {
			formal, tail = "arguments...", "arguments"
		}
		// A separate, unprojected selected-compiler action is the reference.
		// It neither reads nor reuses the managed query source or its answer.
		request := kconfig.ProbeRequest{
			Schema: kconfig.LinuxProbeRequestSchema,
			Steps: []kconfig.ProbeStep{{
				Name: "direct-variadic", Tool: "cc",
				Arguments: append(slices.Clone(test.arguments()), "-E", "-P", "-x", "c", "-"),
				Stdin:     "#define DIRECT_EMPTY_VARIADIC(" + formal + ") 17, ##" + tail + " 23\nDIRECT_EMPTY_VARIADIC()\n",
			}},
			Outcome: kconfig.ProbeOutcome{Kind: "boolean", Predicate: &kconfig.ProbePredicate{Operator: "exit-zero", Step: "direct-variadic"}},
		}
		direct, err := builder.Request("target", request)
		if err != nil {
			return nil, err
		}
		f.variadicCommas[test.name] = variadicCommaReferences{managed, direct}
		directTerminals = append(directTerminals, direct)
	}
	managedPlan, err := batch.Plan()
	if err != nil {
		return nil, err
	}
	directPlan, err := builder.Plan(directTerminals...)
	if err != nil {
		return nil, err
	}
	return kconfig.MergeProbePlans([]kconfig.ProbePlanVariant{{Name: "managed", Plan: managedPlan}, {Name: "direct", Plan: directPlan}})
}

func (f *fixture) verifyVariadicCommas(oracle *kconfig.ProbeResultOracle) (map[string]variadicCommaResult, error) {
	batch, err := kconfig.NewKbuildCompilerGuardBatch(f.scopes, oracle)
	if err != nil {
		return nil, err
	}
	report := map[string]variadicCommaResult{}
	for _, test := range variadicCommaCases() {
		deleted, state, ref, err := batch.OptionalCompilerVariadicCommaAttempt("target", "cc", "c", test.arguments(), nil, test.syntax, nil)
		if err != nil {
			return nil, err
		}
		refs := f.variadicCommas[test.name]
		if ref != refs.managed || ref.NodeID == refs.direct.NodeID {
			return nil, fmt.Errorf("variadic attempt lost its independent identity")
		}
		direct, err := oracle.Result(refs.direct)
		if err != nil {
			return nil, err
		}
		if direct.Boolean == nil || len(direct.Steps) != 1 {
			return nil, fmt.Errorf("missing direct compiler variadic result")
		}
		step := direct.Steps[0]
		entry := variadicCommaResult{NodeID: ref.NodeID, DirectNodeID: refs.direct.NodeID, ExitCode: step.ExitCode}
		if test.rejected {
			if state != kconfig.OptionalCompilerVariadicCommaUnqueryable || deleted || *direct.Boolean || step.Status != "failure" || step.ExitCode < 1 || step.ExitCode > 255 || !strings.Contains(step.Stderr, "UNRELATED") {
				return nil, fmt.Errorf("variadic probe erased unused conflicting macro diagnostics")
			}
		} else {
			if state != kconfig.OptionalCompilerVariadicCommaAnswered || !*direct.Boolean || step.Status != "success" || step.ExitCode != 0 {
				return nil, fmt.Errorf("%s did not measure both configured compiler invocations", test.name)
			}
			// Read the direct spelling, not a compiler-family expectation table.
			text := strings.Join(strings.Fields(strings.ReplaceAll(step.Stdout, ",", " , ")), " ")
			if text != "17 23" && text != "17 , 23" || deleted != (text == "17 23") {
				return nil, fmt.Errorf("%s managed answer disagrees with direct compiler output %q", test.name, step.Stdout)
			}
			entry.DeleteComma = &deleted
		}
		report[test.name] = entry
	}
	if _, err := batch.Plan(); err != nil {
		return nil, err
	}
	return report, nil
}
