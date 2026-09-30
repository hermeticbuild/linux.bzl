package kconfig

import (
	"reflect"
	"strings"
	"testing"
)

func variadicCommaTestRequest(t *testing.T, syntax string) ProbeRequest {
	t.Helper()
	request := intrinsicSourceShapeTestRequest()
	source, err := compilerVariadicCommaSource(syntax)
	if err != nil {
		t.Fatal(err)
	}
	request.Steps[0].Name, request.Steps[0].Stdin = compilerVariadicCommaStep, source
	request.Outcome.Step = compilerVariadicCommaStep
	return request
}

func TestCompilerVariadicCommaResultGrammar(t *testing.T) {
	for _, test := range []struct {
		text   string
		delete bool
	}{
		{"17 23", true}, {"\n17\t23\n", true}, {"17,23", false},
		{" 17 \v,\f23\r\n", false},
	} {
		got, err := parseCompilerVariadicCommaResult(test.text)
		if err != nil || got != test.delete {
			t.Fatalf("%q: %t, %v", test.text, got, err)
		}
	}
	for _, text := range []string{"", "17", "23", "1723", "1 7 23", "17 2 3", "17,,23", "17,23;", "17 23 17 23", "17\u00a023", "17/**/23", "17 + 23", strings.Repeat(" ", 129) + "17 23"} {
		if _, err := parseCompilerVariadicCommaResult(text); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
}

func TestCompilerVariadicCommaExactManagedShape(t *testing.T) {
	for _, syntax := range []string{"standard", "named"} {
		request := variadicCommaTestRequest(t, syntax)
		if err := request.Validate(); err != nil {
			t.Fatal(err)
		}
		if got, err := compilerVariadicCommaProbeSyntax(request.Steps[0]); err != nil || got != syntax {
			t.Fatalf("syntax = %s, %v", got, err)
		}
	}
	for _, change := range []func(*ProbeRequest){
		func(r *ProbeRequest) { r.Steps[0].Stdin += "_Pragma(P)\n" },
		func(r *ProbeRequest) { r.Steps[0].Stdin = strings.ReplaceAll(r.Steps[0].Stdin, "17", "42") },
		func(r *ProbeRequest) {
			r.Steps[0].Stdin = strings.ReplaceAll(r.Steps[0].Stdin, "#error probe_name_collision", "#undef "+compilerVariadicCommaMacro)
		},
		func(r *ProbeRequest) {
			r.Steps[0].Name = "compiler-intrinsic-integer"
			r.Outcome.Step = r.Steps[0].Name
		},
		func(r *ProbeRequest) { r.Steps[0].Tool = "ld" },
		func(r *ProbeRequest) { r.Steps[0].Arguments[2] = "-c" },
		func(r *ProbeRequest) { r.Steps[0].Candidate.Base = []int{0} },
		func(r *ProbeRequest) { r.Steps[0].Candidate.Base = []int{0, 1, 2} },
		func(r *ProbeRequest) { intrinsicSourceShapeConditional(r, 3, true) },
	} {
		request := variadicCommaTestRequest(t, "standard")
		change(&request)
		if err := request.Validate(); err == nil {
			t.Fatal("accepted altered managed source/argv")
		}
		if _, err := ValidateCompilerIntrinsicProbeCandidateArguments(request.Steps[0], []string{"-nostdinc"}); err == nil {
			t.Fatal("runtime accepted altered managed source/argv")
		}
	}
	step := variadicCommaTestRequest(t, "standard").Steps[0]
	for _, argv := range [][]string{
		{"-D" + compilerVariadicCommaMacro + "=1"}, {"-D", compilerVariadicCommaMacro + "(x)=x"},
		{"-U" + compilerVariadicCommaMacro}, {"-U", compilerVariadicCommaMacro}, {"-Wp,-DOTHER=1"},
	} {
		if _, err := ValidateCompilerIntrinsicProbeCandidateArguments(step, argv); err == nil {
			t.Fatalf("accepted probe binding override %q", argv)
		}
	}
	for _, argv := range [][]string{{"-std=c11"}, {"-std=gnu11", "-DOTHER=73"}, {"-D__has_attribute(x)=7"}} {
		if _, err := ValidateCompilerIntrinsicProbeCandidateArguments(step, argv); err != nil {
			t.Fatalf("lost original unrelated compiler flags %q: %v", argv, err)
		}
	}
}

func TestCompilerVariadicCommaDetachedIdentityAndReplay(t *testing.T) {
	scopes := compilerGuardBatchScopesForTest(t, compilerDefinednessTestOptions(t))
	before := compilerGuardBatchOrdinaryPlanForTest(t, scopes)
	discovery, err := NewKbuildCompilerGuardBatch(scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-nostdinc", "-std=c11", "-DOTHER=73", "-include", "forced.h", "source.c"}
	units := []string{"source.c"}
	query := func(batch *KbuildCompilerGuardBatch) (bool, OptionalCompilerVariadicCommaState, ProbeReference, error) {
		return batch.OptionalCompilerVariadicCommaAttempt("target", "cc", "c", args, units, "standard", nil)
	}
	deleted, state, ref, err := query(discovery)
	if err != nil || deleted || state != OptionalCompilerVariadicCommaPending || ref.NodeID == "" {
		t.Fatalf("discovery fabricated facts: %t %v %#v %v", deleted, state, ref, err)
	}
	_, _, repeat, err := query(discovery)
	if err != nil || repeat != ref {
		t.Fatal("identical query changed identity", err)
	}
	plan, err := discovery.Plan()
	if err != nil || len(plan.Nodes) != 1 || len(plan.Terminal) != 1 {
		t.Fatal("invalid detached plan", err)
	}
	if !reflect.DeepEqual(before, compilerGuardBatchOrdinaryPlanForTest(t, scopes)) {
		t.Fatal("changed ordinary discovery")
	}
	for _, name := range []string{"deleted", "retained", "rejected", "rejected malformed", "missing", "stale", "toolset", "signal", "skipped", "extra step", "spoof", "malformed"} {
		t.Run(name, func(t *testing.T) {
			oracle := optionalCompilerDefinednessOracleForTest(t, plan, map[string]string{compilerVariadicCommaStep: "17 23\n"})
			result := oracle.results[ref.NodeID]
			wantState, wantDelete, wantError := OptionalCompilerVariadicCommaPending, false, true
			switch name {
			case "deleted":
				wantState, wantDelete, wantError = OptionalCompilerVariadicCommaAnswered, true, false
			case "retained":
				wantState, wantError = OptionalCompilerVariadicCommaAnswered, false
				result.Steps[0].Stdout = "17,23\n"
			case "rejected", "rejected malformed":
				wantState, wantError = OptionalCompilerVariadicCommaUnqueryable, false
				*result.Boolean = false
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
				if name == "rejected malformed" {
					result.Steps[0].Stdout = "counterfeit"
				}
			case "stale":
				result.RequestID = strings.Repeat("f", 64)
			case "toolset":
				result.ToolsetIdentity = "sha256-" + strings.Repeat("f", 64)
			case "signal":
				*result.Boolean = false
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", -1
			case "skipped":
				*result.Boolean = false
				result.Steps[0].Status, result.Steps[0].ExitCode = "skipped", -1
			case "extra step":
				result.Steps = append(result.Steps, ProbeStepResult{Name: "extra", Status: "success"})
			case "spoof":
				result.Steps[0].Status, result.Steps[0].ExitCode = "failure", 1
			case "malformed":
				result.Steps[0].Stdout = "1723"
			}
			oracle.results[ref.NodeID] = result
			if name == "missing" {
				delete(oracle.results, ref.NodeID)
			}
			batch, err := NewKbuildCompilerGuardBatch(scopes, oracle)
			if err != nil {
				t.Fatal(err)
			}
			deleted, state, gotRef, err := query(batch)
			if (err != nil) != wantError || deleted != wantDelete || state != wantState ||
				wantError && gotRef != (ProbeReference{}) || !wantError && gotRef != ref {
				t.Fatalf("query = %t %v %#v %v", deleted, state, gotRef, err)
			}
			if _, err := batch.Plan(); (err != nil) != wantError {
				t.Fatal("failed authentication did not poison batch", err)
			}
		})
	}
	for _, test := range []struct{ syntax, standard string }{{"named", "c11"}, {"standard", "gnu11"}} {
		batch, err := NewKbuildCompilerGuardBatch(scopes, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, other, err := batch.OptionalCompilerVariadicCommaAttempt("target", "cc", "c", []string{"-nostdinc", "-std=" + test.standard, "-DOTHER=73", "-include", "forced.h", "source.c"}, units, test.syntax, nil)
		if err != nil || other.NodeID == ref.NodeID {
			t.Fatal("different syntax/dialect borrowed identity", err)
		}
	}
}
