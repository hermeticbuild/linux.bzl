package main

import (
	"fmt"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

type macroWriteResult struct {
	NodeID   string `json:"node_id"`
	Success  bool   `json:"success"`
	ExitCode int    `json:"exit_code"`
}

func macroWriteCases() []struct {
	name, definition string
	success          bool
} {
	return []struct {
		name, definition string
		success          bool
	}{
		{"conflicting", "#define CONFIG_COLLISION 2\n", false},
		{"identical", "#define CONFIG_COLLISION 1\n", true},
		{"omitted", "", true},
	}
}

// An unread CONFIG definition still affects compilation: dropping a conflicting
// autoconf entry would incorrectly turn a -Werror failure into success. Execute
// all cases through the same configured compiler and production probe callback;
// no compiler-family facts or expected diagnostic wording are synthesized.
func (f *fixture) discoverMacroWrites() (*kconfig.ProbePlan, error) {
	builder, err := kconfig.NewProbePlanBuilder(f.toolset, "")
	if err != nil {
		return nil, err
	}
	f.macroWrites = map[string]kconfig.ProbeReference{}
	var terminals []kconfig.ProbeReference
	for _, test := range macroWriteCases() {
		request := kconfig.ProbeRequest{
			Schema: kconfig.LinuxProbeRequestSchema,
			Scratch: []kconfig.ProbeScratch{
				{Name: "autoconf", Kind: "file", Content: test.definition, Present: true},
				{Name: "object", Kind: "file"},
			},
			Steps: []kconfig.ProbeStep{{
				Name: "compile", Tool: "cc",
				Arguments: []string{"-nostdinc", "-Werror", "-include", "${scratch:autoconf}", "-c", "-x", "c", "-", "-o", "${scratch:object}"},
				Stdin:     "#define RAW(x) #x\nconst char raw[] = RAW(CONFIG_RAW);\n#define CONFIG_COLLISION 1\nint value;\n",
			}},
			Outcome: kconfig.ProbeOutcome{Kind: "boolean", Predicate: &kconfig.ProbePredicate{
				Operator: "all", Operands: []kconfig.ProbePredicate{
					{Operator: "exit-zero", Step: "compile"},
					{Operator: "regular-file", Scratch: "object"},
				},
			}},
		}
		ref, err := builder.Request("target", request)
		if err != nil {
			return nil, err
		}
		f.macroWrites[test.name] = ref
		terminals = append(terminals, ref)
	}
	return builder.Plan(terminals...)
}

func (f *fixture) verifyMacroWrites(oracle *kconfig.ProbeResultOracle) (map[string]macroWriteResult, error) {
	report := map[string]macroWriteResult{}
	for _, test := range macroWriteCases() {
		ref := f.macroWrites[test.name]
		result, err := oracle.Result(ref)
		if err != nil {
			return nil, err
		}
		if result.Boolean == nil || *result.Boolean != test.success || len(result.Steps) != 1 {
			return nil, fmt.Errorf("%s macro write has unexpected compiler outcome", test.name)
		}
		step := result.Steps[0]
		if test.success {
			if step.Status != "success" || step.ExitCode != 0 {
				return nil, fmt.Errorf("%s macro write did not compile", test.name)
			}
		} else if step.Status != "failure" || step.ExitCode < 1 || step.ExitCode > 255 || !strings.Contains(step.Stderr, "CONFIG_COLLISION") {
			return nil, fmt.Errorf("conflicting macro write lacks a compiler diagnostic for CONFIG_COLLISION")
		}
		report[test.name] = macroWriteResult{ref.NodeID, test.success, step.ExitCode}
	}
	return report, nil
}
