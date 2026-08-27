package kconfig

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestKbuildCompoundTryRunPreservesSourceProgramAndPriorProbeInputs(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	evaluator.tools[linuxProbeScriptRunner] = "/configured/scriptrun"
	evaluator.tools[linuxProbeScriptRuntime] = "/configured/script-runtime"
	evaluator.tools["cc-link"] = evaluator.tools["cc"]

	wrapper := func(body, success, failure string) string {
		return `set -e; TMP=.tmp_$$/tmp; trap "rm -rf .tmp_$$" EXIT; mkdir -p .tmp_$$; if (` + body + `) >/dev/null 2>&1; then echo "` + success + `"; else echo "` + failure + `"; fi`
	}
	prior, err := evaluator.KbuildShell(context.Background(), wrapper(
		`/configured/clang -Werror -fprior-capability -c -x c /dev/null -o "$TMP"`,
		"-I"+linuxProbeHostDepsSentinel+"/include", "",
	))
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(prior) {
		t.Fatalf("prior capability = %q, want symbolic probe", prior)
	}
	body := `echo 'long long x; void f(void){x++;}' | /configured/clang ` + prior + ` -w -fprofile-arcs -ftest-coverage -x c - -c -o "$TMP.base" && ` +
		`echo 'long long x; void f(void){x++;}' | /configured/clang ` + prior + ` -w -fprofile-arcs -ftest-coverage -fprofile-update=prefer-atomic -x c - -c -o "$TMP" && ` +
		`/configured/llvm-nm "$TMP.base" | grep ' U ' > "$TMP.ubase" || true ; ` +
		`/configured/llvm-nm "$TMP" | grep ' U ' > "$TMP.utest" || true ; ` +
		`cmp -s "$TMP.ubase" "$TMP.utest"`
	selected, err := evaluator.KbuildShell(context.Background(), wrapper(body, " -fprofile-update=prefer-atomic", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(selected) {
		t.Fatalf("compound capability = %q, want symbolic probe", selected)
	}

	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plan.Nodes), 2; got != want {
		t.Fatalf("probe nodes = %d, want %d: %#v", got, want, plan.Nodes)
	}
	compound := plan.Nodes[1]
	if got, want := compound.Inputs, []string{plan.Nodes[0].ID}; !slices.Equal(got, want) {
		t.Fatalf("compound inputs = %v, want %v", got, want)
	}
	request := plan.Requests[compound.RequestID]
	if request.InputCount != 1 || len(request.Steps) != 1 {
		t.Fatalf("compound request shape = %#v", request)
	}
	step := request.Steps[0]
	if step.Tool != linuxProbeScriptRunner || step.Stdin != "" || len(step.StdinFragments) == 0 || len(step.Environment) != 0 ||
		!step.DiscardStdout || !step.DiscardStderr {
		t.Fatalf("compound step = %#v", step)
	}
	if got, want := step.AuxiliaryTools, []string{"cc", "nm"}; !slices.Equal(got, want) {
		t.Fatalf("compound auxiliary tools = %v, want %v", got, want)
	}
	if got, want := request.ToolRoles(), []string{"cc", "nm", "script-runtime", "scriptrun"}; !slices.Equal(got, want) {
		t.Fatalf("compound tool roles = %v, want %v", got, want)
	}
	if got, want := request.SourceRoots, []string{linuxProbeHostDepsRootName, linuxProbeSourceRootName}; !slices.Equal(got, want) {
		t.Fatalf("compound source roots = %v, want %v", got, want)
	}
	encoded, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(encoded)
	for _, want := range []string{
		`"-script_stdin"`, `"-require_applet","cmp"`, `"-require_applet","grep"`,
		`"tool":"scriptrun"`, `"operator":"result-true","result":"00000000"`,
		`-fprofile-update=prefer-atomic`, `TMP=\"$1\"; shift`, `"--","${scratch:tmp}"`,
		`"discard_stdout":true`, `"discard_stderr":true`, `-I${source_root:host_deps}/include`,
	} {
		if !strings.Contains(serialized, want) {
			t.Errorf("compound request omits %q: %s", want, serialized)
		}
	}
	if strings.Contains(serialized, linuxProbeSymbolPrefix) || strings.Contains(serialized, linuxProbeHostDepsSentinel) ||
		strings.Contains(serialized, "/configured/clang") || strings.Contains(serialized, "/configured/llvm-nm") {
		t.Fatalf("compound request leaked planner/tool spelling: %s", serialized)
	}
}

func TestKbuildCompoundTryRunRejectsSymbolicShellOperators(t *testing.T) {
	builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	evaluator.tools[linuxProbeScriptRunner] = "/configured/scriptrun"
	evaluator.tools[linuxProbeScriptRuntime] = "/configured/script-runtime"
	wrapper := func(body, success string) string {
		return `set -e; TMP=.tmp_$$/tmp; trap "rm -rf .tmp_$$" EXIT; mkdir -p .tmp_$$; if (` + body + `) >/dev/null 2>&1; then echo "` + success + `"; else echo ""; fi`
	}
	injected, err := evaluator.KbuildShell(context.Background(), wrapper(
		`/configured/clang -Werror -fsource-selected -c -x c /dev/null -o "$TMP"`,
		`; true`,
	))
	if err != nil {
		t.Fatal(err)
	}
	body := `/configured/clang ` + injected + ` -x c - -c -o "$TMP" && true`
	if _, err := evaluator.KbuildShell(context.Background(), wrapper(body, "y")); err == nil || !strings.Contains(err.Error(), "introduces shell operator") {
		t.Fatalf("KbuildShell() error = %v, want symbolic shell-operator rejection", err)
	}
}

func TestKbuildCompoundTryRunRejectsLiteralProbePlaceholder(t *testing.T) {
	builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	evaluator.tools[linuxProbeScriptRunner] = "/configured/scriptrun"
	evaluator.tools[linuxProbeScriptRuntime] = "/configured/script-runtime"
	command := `set -e; TMP=.tmp_$$/tmp; trap "rm -rf .tmp_$$" EXIT; mkdir -p .tmp_$$; if (` +
		`echo '${tool:cc}' | /configured/clang -x c - -c -o "$TMP" && true` +
		`) >/dev/null 2>&1; then echo "y"; else echo "n"; fi`
	if _, err := evaluator.KbuildShell(context.Background(), command); err == nil || !strings.Contains(err.Error(), "collides with probe placeholder syntax") {
		t.Fatalf("KbuildShell() error = %v, want placeholder rejection", err)
	}
}

func TestKbuildCompoundTryRunRejectsNonPrivateRedirection(t *testing.T) {
	builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	evaluator.tools[linuxProbeScriptRunner] = "/configured/scriptrun"
	evaluator.tools[linuxProbeScriptRuntime] = "/configured/script-runtime"
	command := `set -e; TMP=.tmp_$$/tmp; trap "rm -rf .tmp_$$" EXIT; mkdir -p .tmp_$$; if (` +
		`/configured/clang -x c - -c -o "$TMP" && /configured/llvm-nm "$TMP" > outside` +
		`) >/dev/null 2>&1; then echo "y"; else echo "n"; fi`
	if _, err := evaluator.KbuildShell(context.Background(), command); err == nil || !strings.Contains(err.Error(), "redirects outside private TMP") {
		t.Fatalf("KbuildShell() error = %v, want private-TMP rejection", err)
	}
}

func TestCompactKbuildRecipeLexerRecognizesOutputRedirection(t *testing.T) {
	tokens, err := lexCompactKbuildRecipe(`grep ' U ' > "$TMP.undefined"`)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(tokens))
	for _, token := range tokens {
		got = append(got, token.value)
	}
	if want := []string{"grep", " U ", ">", "$TMP.undefined"}; !slices.Equal(got, want) || !tokens[2].operator {
		t.Fatalf("tokens = %#v, want %v with output operator", tokens, want)
	}
}
