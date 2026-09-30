package kconfig

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

const dynamicExprFakeStatusEnvironment = "LINUX_BZL_DYNAMIC_EXPR_FAKE_STATUS"

func TestMain(m *testing.M) {
	if os.Getenv(dynamicExprFakeStatusEnvironment) == "3" && filepath.Base(os.Args[0]) == "expr" {
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func dynamicExprFixture(t *testing.T) (*LinuxProbeEvaluator, *ProbePlanBuilder, string, ProbeReference) {
	t.Helper()
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	evaluator.tools[linuxProbeScriptRunner] = "/configured/scriptrun"
	evaluator.tools[linuxProbeScriptRuntime] = "/configured/busybox"
	evaluator.tools[compactKbuildScriptAppletRolePrefix+"expr"] = "/configured/expr"
	prior, err := evaluator.requestText(ProbeRequest{
		Schema:  LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "measure", Tool: "cc", Arguments: []string{"--version"}}},
		Outcome: ProbeOutcome{Kind: "text", Step: "measure", Stream: "stdout", FirstLine: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return evaluator, builder, prior, evaluator.symbols[prior].reference
}

func TestDynamicShellExprPreservesMeasuredTextDependency(t *testing.T) {
	evaluator, builder, prior, priorReference := dynamicExprFixture(t)
	value, err := evaluator.Shell(context.Background(), `expr `+prior+` \< 16`)
	if err != nil || !exactLinuxProbeSymbol(value) {
		t.Fatalf("dynamic expr = %q, err=%v", value, err)
	}
	symbol := evaluator.symbols[value]
	if got, want := symbol.dependencies, []ProbeReference{priorReference}; !slices.Equal(got, want) {
		t.Fatalf("dynamic expr dependencies = %#v, want %#v", got, want)
	}
	request := symbol.request
	if request.InputCount != 1 || len(request.Steps) != 1 {
		t.Fatalf("dynamic expr request = %#v", request)
	}
	if got, want := request.ToolRoles(), []string{"script-applet-expr", "script-runtime", "scriptrun"}; !slices.Equal(got, want) {
		t.Fatalf("dynamic expr roles = %q, want %q", got, want)
	}
	step := request.Steps[0]
	if step.Tool != linuxProbeScriptRunner || !slices.Equal(step.AuxiliaryTools, []string{"script-applet-expr"}) {
		t.Fatalf("dynamic expr step = %#v", step)
	}
	joined := strings.Join(step.Arguments, " ")
	for _, required := range []string{
		"-script_content " + strings.TrimSpace(linuxProbeDynamicExprScript),
		"-applet expr=${tool:script-applet-expr}",
		"-require_applet expr", "-require_applet sh", "--  < 16",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("dynamic expr argv %q omits %q", joined, required)
		}
	}
	if len(step.ArgumentFragments) != 1 {
		t.Fatalf("dynamic expr argument fragments = %#v", step.ArgumentFragments)
	}
	fragment := step.ArgumentFragments[0]
	if fragment.Mode != ProbeArgumentFragmentsModeSignedDecimal ||
		fragment.Index >= len(step.Arguments) || step.Arguments[fragment.Index] != "" ||
		len(fragment.Fragments) != 1 || fragment.Fragments[0].Value != "${result:00000000.text}" {
		t.Fatalf("dynamic expr measured argv slot = %#v in %q", fragment, step.Arguments)
	}
	if got, want := request.Outcome, (ProbeOutcome{
		Kind: "text", Step: "dynamic-expr", Stream: "stdout", GNUMakeShell: true, RequireSuccess: true,
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("dynamic expr outcome = %#v, want %#v", got, want)
	}
	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 2 || !slices.Equal(plan.Nodes[1].Inputs, []string{plan.Nodes[0].ID}) {
		t.Fatalf("dynamic expr plan = %#v, want measured-text edge", plan.Nodes)
	}
}

func TestDynamicShellExprTypesEveryMeasuredOperand(t *testing.T) {
	evaluator, _, left, leftReference := dynamicExprFixture(t)
	right, err := evaluator.requestText(ProbeRequest{
		Schema:  LinuxProbeRequestSchema,
		Steps:   []ProbeStep{{Name: "measure", Tool: "cc", Arguments: []string{"-dumpversion"}}},
		Outcome: ProbeOutcome{Kind: "text", Step: "measure", Stream: "stdout", FirstLine: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	rightReference := evaluator.symbols[right].reference
	value, err := evaluator.Shell(context.Background(), "expr "+left+" + "+right)
	if err != nil {
		t.Fatal(err)
	}
	symbol := evaluator.symbols[value]
	if got, want := symbol.dependencies, []ProbeReference{leftReference, rightReference}; !slices.Equal(got, want) {
		t.Fatalf("dynamic expr dependencies = %#v, want %#v", got, want)
	}
	step := symbol.request.Steps[0]
	if len(step.ConditionalArguments) != 0 || len(step.ArgumentFragments) != 2 {
		t.Fatalf("dynamic expr lowering = conditional %#v, fragments %#v", step.ConditionalArguments, step.ArgumentFragments)
	}
	for index, wantIndex := range []int{len(step.Arguments) - 3, len(step.Arguments) - 1} {
		group := step.ArgumentFragments[index]
		if group.Index != wantIndex || group.Mode != ProbeArgumentFragmentsModeSignedDecimal {
			t.Errorf("dynamic expr fragment %d = %#v, want index %d signed-decimal", index, group, wantIndex)
		}
	}
}

func TestDynamicExprScriptPreservesExprStatusContract(t *testing.T) {
	scriptRunner, scriptRuntime := dynamicExprScriptTestRunfiles(t)
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, stdout, stderr string
		arguments            []string
		overrideExpr         bool
		wantFailure          bool
	}{
		{name: "false status one", arguments: []string{"1", "=", "2"}, stdout: "0\n"},
		{name: "division status two", arguments: []string{"1", "/", "0"}, stderr: "division by zero"},
		{name: "override status three", arguments: []string{"1", "=", "1"}, overrideExpr: true, wantFailure: true, stderr: "exit status 3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			arguments := []string{
				"-interpreter", scriptRuntime,
				"-interpreter_arg", "sh",
				"-multicall", scriptRuntime,
				"-script_content", linuxProbeDynamicExprScript,
			}
			if test.overrideExpr {
				arguments = append(arguments, "-applet", "expr="+testExecutable)
			}
			arguments = append(arguments, "-require_applet", "expr", "-require_applet", "sh", "--")
			arguments = append(arguments, test.arguments...)
			command := exec.Command(scriptRunner, arguments...)
			command.Env = os.Environ()
			if test.overrideExpr {
				command.Env = append(command.Env, dynamicExprFakeStatusEnvironment+"=3")
			}
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			runErr := command.Run()
			if test.wantFailure != (runErr != nil) {
				t.Fatalf("scriptrun error = %v, stderr = %q; want failure %t", runErr, stderr.String(), test.wantFailure)
			}
			if got := stdout.String(); got != test.stdout {
				t.Errorf("stdout = %q, want %q", got, test.stdout)
			}
			if test.stderr != "" && !strings.Contains(stderr.String(), test.stderr) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), test.stderr)
			}
		})
	}
}

func dynamicExprScriptTestRunfiles(t *testing.T) (string, string) {
	t.Helper()
	logicalRunner := os.Getenv("LINUX_BZL_TEST_SCRIPTRUN")
	logicalRuntime := os.Getenv("LINUX_BZL_TEST_SCRIPT_RUNTIME")
	if logicalRunner == "" && logicalRuntime == "" {
		t.Skip("declared scriptrun and BusyBox runfiles are unavailable under direct go test")
	}
	if logicalRunner == "" || logicalRuntime == "" {
		t.Fatalf("incomplete dynamic expr runfiles: scriptrun=%q runtime=%q", logicalRunner, logicalRuntime)
	}
	runner, err := runfiles.Rlocation(logicalRunner)
	if err != nil {
		t.Fatalf("resolve scriptrun runfile %q: %v", logicalRunner, err)
	}
	runtime, err := runfiles.Rlocation(logicalRuntime)
	if err != nil {
		t.Fatalf("resolve script runtime runfile %q: %v", logicalRuntime, err)
	}
	return runner, runtime
}

func TestDynamicShellExprRejectsFiniteMeasuredOperands(t *testing.T) {
	for _, kind := range []string{"boolean", "selection"} {
		t.Run(kind, func(t *testing.T) {
			evaluator, _, _, _ := dynamicExprFixture(t)
			token := linuxProbeSymbolPrefix + strings.Repeat(map[string]string{"boolean": "a", "selection": "b"}[kind], 64)
			evaluator.symbols[token] = linuxProbeSymbol{kind: kind, trueText: "1 : .*", falseText: "0", selectionValues: []string{"0", "1"}}
			if value, recognized, err := evaluator.dynamicShellExpr("expr " + token + " + 1"); err == nil || !recognized {
				t.Fatalf("dynamic expr finite operand = %q, recognized=%v, err=%v", value, recognized, err)
			}
		})
	}
}

func TestDynamicShellExprRejectsAmbientOrDifferentShellGrammar(t *testing.T) {
	for _, command := range []string{
		`expr %s < 16`,
		`expr '%s' \< 16`,
		`expr prefix%s \< 16`,
		`expr %s : '.*'`,
		`expr %s \< /tmp/value`,
		`expr %s \< 16 + 1`,
		`expr %s \< 16 ; id`,
		`expr %s \< 16 | sed -n p`,
		`expr %s + +1`,
		`expr +1 + %s`,
	} {
		t.Run(command, func(t *testing.T) {
			evaluator, _, prior, _ := dynamicExprFixture(t)
			command := strings.ReplaceAll(command, "%s", prior)
			if value, recognized, err := evaluator.dynamicShellExpr(command); err == nil || !recognized {
				t.Fatalf("dynamic expr %q = %q, recognized=%v, err=%v; want owned rejection", command, value, recognized, err)
			}
			if got := len(evaluator.References()); got != 1 {
				t.Fatalf("rejected dynamic expr emitted %d additional requests", got-1)
			}
		})
	}
}

func TestDynamicShellExprLeavesConcreteArithmeticToPureEvaluator(t *testing.T) {
	evaluator, _, _, _ := dynamicExprFixture(t)
	if value, recognized, err := evaluator.dynamicShellExpr(`expr 2 + 3 \* 4`); err != nil || recognized || value != "" {
		t.Fatalf("concrete dynamic expr = %q, recognized=%v, err=%v", value, recognized, err)
	}
}
