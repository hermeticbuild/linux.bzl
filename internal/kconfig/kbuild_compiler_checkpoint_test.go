package kconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func compilerCheckpointKindsFixture(t *testing.T) (KbuildProbeWorkloadOptions, *KbuildProbeScopes, func(bool) func(string) (string, error), string) {
	t.Helper()
	options := compilerDefinednessTestOptions(t)
	host := testKbuildProbeScopeOptions(t, linuxCompilerBootstrapFixtures(t)[0])
	options.Host = &host
	s := compilerGuardBatchScopesForTest(t, options)
	e := s.evaluators["target"]
	text, err := e.requestText(ProbeRequest{Schema: LinuxProbeRequestSchema, Steps: []ProbeStep{{Name: "flags", Tool: "cc", Arguments: []string{"--version"}}}, Outcome: ProbeOutcome{Kind: "text", Step: "flags", Stream: "stdout", RequireSuccess: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.renderTextTransform(text, "addprefix", []string{"-I", text}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.renderMakeText("strip", []string{text}, "", linuxProbeMakeTextProtocolUnusable); err != nil {
		t.Fatal(err)
	}
	if _, err := e.renderSourceShellWords(text); err != nil {
		t.Fatal(err)
	}
	h := s.evaluators["host"]
	truth, err := h.requestTruth(ProbeRequest{Schema: LinuxProbeRequestSchema, Steps: []ProbeStep{{Name: "select", Tool: "cc", Arguments: []string{"--version"}}}, Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "select"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.renderTruth(truth, "yes", "no"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.renderSelection([]linuxProbeSelectionInput{{truth.reference, truth.request, truth.dependencies}}, []string{"off", "on"}); err != nil {
		t.Fatal(err)
	}
	// A separately measured Kconfig path enters as a literal, never as an old
	// registry's authorization set. Each restorer gets a new upstream codec.
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	request := ProbeRequest{Schema: LinuxProbeRequestSchema, Steps: []ProbeStep{{Name: "path", Tool: "cc", Arguments: []string{"-print-file-name=include"}, StdoutExecrootRelative: true, StdoutFallbackPath: "include"}}, Outcome: ProbeOutcome{Kind: "text", Step: "path", Stream: "stdout", TrimSpace: true}}
	ref, err := builder.Request("target", request)
	if err != nil {
		t.Fatal(err)
	}
	const pathname = "external/compiler/current-include"
	result := ProbeResult{Schema: LinuxProbeResultSchema, NodeID: ref.NodeID, RequestID: ref.RequestID, Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: pathname, Steps: []ProbeStepResult{{Name: "path", Status: "success", Stdout: pathname, StdoutPathKind: ProbeStdoutPathToolset}}}
	newUpstream := func() *LinuxProbeEvaluator {
		return &LinuxProbeEvaluator{symbolRegistry: newLinuxProbeSymbolRegistry(), oracle: &ProbeResultOracle{toolsets: map[string]string{"target": bootstrapTestIdentity}, results: map[string]ProbeResult{ref.NodeID: result}}}
	}
	upstream := newUpstream()
	capability, err := upstream.readText(ref, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportToolsetPathCapabilities(capability, upstream.NormalizeToolsetPathCapabilities); err != nil {
		t.Fatal(err)
	}
	normalizer := func(replay bool) func(string) (string, error) {
		fresh := newUpstream()
		if replay {
			if _, err := fresh.readText(ref, request); err != nil {
				t.Fatal(err)
			}
		}
		return fresh.NormalizeOrAuthorizeToolsetPathCapabilities
	}
	e.scriptEnvironment = maps.Clone(e.scriptEnvironment)
	e.scriptEnvironment["SOURCE_EXPORTED"] = "exact source value"
	kinds := map[string]bool{}
	for _, symbol := range e.symbolRegistry.symbols {
		kinds[symbol.kind] = true
	}
	if len(kinds) != 7 {
		t.Fatalf("fixture has %d symbol kinds, want all seven", len(kinds))
	}
	return options, s, normalizer, capability
}

func TestCompilerCheckpointRestoresEverySymbolKind(t *testing.T) {
	options, original, normalizer, oldCapability := compilerCheckpointKindsFixture(t)
	data, err := original.MarshalCompilerCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(oldCapability)) {
		t.Fatal("serialized old capability")
	}
	fresh := compilerGuardBatchScopesForTest(t, options)
	if err := fresh.RestoreCompilerCheckpoint(data, normalizer(true)); err != nil {
		t.Fatal(err)
	}
	got, err := fresh.MarshalCompilerCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("restore added aliases or changed exact compiler state")
	}
	if fresh.evaluators["target"].symbolRegistry == original.evaluators["target"].symbolRegistry {
		t.Fatal("retained old registry")
	}
	if _, err := fresh.evaluators["target"].NormalizeToolsetPathCapabilities(oldCapability); err == nil {
		t.Fatal("fresh workload accepted old capability")
	}
}

func TestCompilerCheckpointRejectsMalformedAndUnboundState(t *testing.T) {
	options, original, normalizer, _ := compilerCheckpointKindsFixture(t)
	data, err := original.MarshalCompilerCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"unknown-field", "duplicate-field", "trailing-json", "wrong-schema", "wrong-toolset", "wrong-architecture", "wrong-base-environment", "missing-definition", "mixed-symbol", "wrong-kind", "wrong-token", "forged-literal", "no-authorization", "missing-authorizer"} {
		t.Run(name, func(t *testing.T) {
			var r kbuildCompilerCheckpoint
			if err := json.Unmarshal(data, &r); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "wrong-schema":
				r.Schema += "-other"
			case "wrong-toolset":
				r.Plan.Toolsets["target"] = "sha256-" + strings.Repeat("1", 64)
			case "wrong-architecture":
				v := r.Scopes["target"]
				v.Architecture = "other"
				r.Scopes["target"] = v
			case "wrong-base-environment":
				if r.BaseEnvironments["target"] == nil {
					r.BaseEnvironments["target"] = map[string]string{}
				}
				r.BaseEnvironments["target"]["OUTSIDE"] = "changed"
			case "missing-definition":
				for id := range r.Definitions {
					delete(r.Definitions, id)
					break
				}
			case "mixed-symbol", "wrong-kind", "wrong-token":
				for token, symbol := range r.Symbols {
					if symbol.Kind != "text" {
						continue
					}
					switch name {
					case "mixed-symbol":
						symbol.SourceShellWords = "extra"
					case "wrong-kind":
						symbol.Kind = "unknown"
					case "wrong-token":
						delete(r.Symbols, token)
						token = linuxProbeSymbolPrefix + strings.Repeat("1", 64)
					}
					r.Symbols[token] = symbol
					break
				}
			case "forged-literal":
				for token, symbol := range r.Symbols {
					if symbol.Kind != "toolset-path-literal" {
						continue
					}
					symbol.ToolsetPathLiteral, err = toolaction.EncodeExecutionRootProvenancePath("target", "external/compiler/undeclared")
					if err != nil {
						t.Fatal(err)
					}
					delete(r.Symbols, token)
					digest := sha256.Sum256([]byte("linux-bzl-kconfig-toolset-path-handoff-v1\x00" + symbol.ToolsetPathLiteral))
					r.Symbols[fmt.Sprintf("%s%x", linuxProbeSymbolPrefix, digest)] = symbol
					break
				}
			}
			candidate, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "unknown-field":
				candidate = append([]byte("{\"Other\":0,"), candidate[1:]...)
			case "duplicate-field":
				candidate = append([]byte("{\"Schema\":null,"), candidate[1:]...)
			case "trailing-json":
				candidate = append(candidate, []byte("{}")...)
			}
			fresh := compilerGuardBatchScopesForTest(t, options)
			before := fresh.evaluators["target"].symbolRegistry
			authorize := normalizer(name != "no-authorization")
			if name == "missing-authorizer" {
				authorize = nil
			}
			if err := fresh.RestoreCompilerCheckpoint(candidate, authorize); err == nil {
				t.Fatal("unbound compiler checkpoint accepted")
			}
			if fresh.evaluators["target"].symbolRegistry != before || len(before.symbols) != 0 || len(fresh.References()) != 0 {
				t.Fatal("failed restore published partial compiler state")
			}
		})
	}
}
