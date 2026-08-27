package kconfig

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const selectedToolSourceQueryHeader = "scripts/rust_is_available_bindgen_libclang.h"

func selectedToolSourceQueryFixture(t *testing.T) (*LinuxProbeEvaluator, *ProbePlanBuilder) {
	t.Helper()
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		filepath.Join(root, linuxProbeRootAnchor):                              "mainmenu \"fixture\"\n",
		filepath.Join(root, filepath.FromSlash(selectedToolSourceQueryHeader)): "#include <clang-c/Index.h>\n",
	} {
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	evaluator.sourceRoot = root
	evaluator.tools[linuxProbeScriptRunner] = "/configured/scriptrun"
	evaluator.tools[linuxProbeScriptRuntime] = "/configured/script-runtime"
	evaluator.tools[compactKbuildScriptAppletRolePrefix+"sed"] = "/configured/sed"
	return evaluator, builder
}

func selectedToolSourceQueryCommand() string {
	return KbuildActionRoleToken("target", "bindgen") +
		" " + selectedToolSourceTreePrefix + selectedToolSourceQueryHeader +
		` 2>&1 | sed -ne 's/.*clang version \([0-9]*\).*/\1/p'`
}

func TestSelectedToolSourceQueryCarriesSelectedToolAndImmutableSource(t *testing.T) {
	evaluator, builder := selectedToolSourceQueryFixture(t)
	value, err := evaluator.Shell(context.Background(), selectedToolSourceQueryCommand())
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(value) {
		t.Fatalf("selected-tool source query = %q, want symbolic text", value)
	}

	plan, err := builder.Plan(evaluator.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 || plan.Nodes[0].Scope != "target" {
		t.Fatalf("selected-tool source-query nodes = %#v, want one target node", plan.Nodes)
	}
	request := plan.Requests[plan.Nodes[0].RequestID]
	if request.InputCount != 0 || len(request.Steps) != 1 {
		t.Fatalf("selected-tool source request = %#v", request)
	}
	if got, want := request.Sources, []string{linuxProbeRootAnchor, selectedToolSourceQueryHeader}; !slices.Equal(got, want) {
		t.Fatalf("selected-tool source inputs = %q, want %q", got, want)
	}
	if got, want := request.SourceRoots, []string{linuxProbeSourceRootName}; !slices.Equal(got, want) {
		t.Fatalf("selected-tool source roots = %q, want %q", got, want)
	}
	step := request.Steps[0]
	if step.Name != "selected-tool-source-query" || step.Tool != linuxProbeScriptRunner ||
		step.WorkingDirectory != "${source_root:linux}" {
		t.Fatalf("selected-tool source step = %#v", step)
	}
	if got, want := step.AuxiliaryTools, []string{"bindgen", compactKbuildScriptAppletRolePrefix + "sed"}; !slices.Equal(got, want) {
		t.Fatalf("selected-tool source auxiliary roles = %q, want %q", got, want)
	}
	if got, want := request.ToolRoles(), []string{"bindgen", compactKbuildScriptAppletRolePrefix + "sed", linuxProbeScriptRuntime, linuxProbeScriptRunner}; !slices.Equal(got, want) {
		t.Fatalf("selected-tool source tool roles = %q, want %q", got, want)
	}
	if request.Outcome.Kind != "text" || request.Outcome.Step != step.Name || request.Outcome.Stream != "stdout" ||
		!request.Outcome.GNUMakeShell || !request.Outcome.RequireSuccess || request.Outcome.SingleMakeWord {
		t.Fatalf("selected-tool source outcome = %#v", request.Outcome)
	}

	joined := strings.Join(step.Arguments, " ")
	for _, want := range []string{
		"-script_content",
		`bindgen 'scripts/rust_is_available_bindgen_libclang.h' 2>&1 | sed '-ne' 's/.*clang version \([0-9]*\).*/\1/p'`,
		"-tool bindgen=${tool:bindgen}",
		"-applet sed=${tool:script-applet-sed}",
		"-require_applet sed",
		"-require_applet sh",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("selected-tool source argv %q omits %q", joined, want)
		}
	}
	for _, leak := range []string{"/configured/", selectedToolSourceTreePrefix} {
		if strings.Contains(joined, leak) {
			t.Errorf("selected-tool source argv leaks %q: %q", leak, joined)
		}
	}
}

func TestSelectedToolSourceQueryCanonicalizesPhysicalSourceRoot(t *testing.T) {
	evaluator, _ := selectedToolSourceQueryFixture(t)
	physical := filepath.Join(evaluator.sourceRoot, filepath.FromSlash(selectedToolSourceQueryHeader))
	command := KbuildActionRoleToken("target", "bindgen") + " " + physical +
		` 2>&1 | sed -n -e 's/.*version \([0-9]*\).*/\1/p'`
	if _, err := evaluator.Shell(context.Background(), command); err != nil {
		t.Fatalf("physical-root selected-tool source query: %v", err)
	}
	if got := len(evaluator.References()); got != 1 {
		t.Fatalf("physical-root selected-tool source query references = %d, want 1", got)
	}
}

func TestSelectedToolSourceQueryRejectsBroaderShellAuthority(t *testing.T) {
	evaluator, _ := selectedToolSourceQueryFixture(t)
	evaluator.tools["sed"] = "/configured/producer-sed"
	tool := KbuildActionRoleToken("target", "bindgen")
	source := selectedToolSourceTreePrefix + selectedToolSourceQueryHeader
	validExpression := `'s/.*clang version \([0-9]*\).*/\1/p'`
	tests := map[string]string{
		"escaping source":         tool + " " + selectedToolSourceTreePrefix + "../outside 2>&1 | sed -ne " + validExpression,
		"missing source":          tool + " " + selectedToolSourceTreePrefix + "scripts/missing.h 2>&1 | sed -ne " + validExpression,
		"producer option":         tool + " --output=result " + source + " 2>&1 | sed -ne " + validExpression,
		"spaced stderr merge":     tool + " " + source + " 2 >& 1 | sed -ne " + validExpression,
		"file redirection":        tool + " " + source + " 2>result | sed -ne " + validExpression,
		"extra pipeline":          tool + " " + source + " 2>&1 | sed -ne " + validExpression + " | cat",
		"sed input file":          tool + " " + source + " 2>&1 | sed -ne " + validExpression + " /etc/passwd",
		"sed script file":         tool + " " + source + " 2>&1 | sed -nf /etc/passwd",
		"sed in place":            tool + " " + source + " 2>&1 | sed -i " + validExpression,
		"sed missing e":           tool + " " + source + " 2>&1 | sed -n " + validExpression,
		"sed execute flag":        tool + " " + source + ` 2>&1 | sed -ne 's/.*/touch owned/ep'`,
		"sed write flag":          tool + " " + source + ` 2>&1 | sed -ne 's/.*/owned/wp'`,
		"sed empty state":         tool + " " + source + ` 2>&1 | sed -ne 's//value/p'`,
		"sed placeholder":         tool + " " + source + ` 2>&1 | sed -ne 's/.*/${tool:cc}/p'`,
		"unquoted expression":     tool + " " + source + ` 2>&1 | sed -ne s/.*/value/p`,
		"shell statement":         tool + " " + source + " 2>&1 | sed -ne " + validExpression + "; touch owned",
		"infrastructure producer": KbuildActionRoleToken("target", linuxProbeScriptRuntime) + " " + source + " 2>&1 | sed -ne " + validExpression,
		"reducer collision":       KbuildActionRoleToken("target", "sed") + " " + source + " 2>&1 | sed -ne " + validExpression,
	}
	for name, command := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := evaluator.Shell(context.Background(), command); err == nil {
				t.Fatalf("unsafe selected-tool source query unexpectedly succeeded: %q", command)
			}
			if got := len(evaluator.References()); got != 0 {
				t.Fatalf("unsafe selected-tool source query registered %d requests", got)
			}
		})
	}
}

func TestSelectedToolSourceQueryDoesNotClaimDirectToolQueries(t *testing.T) {
	evaluator, _ := selectedToolSourceQueryFixture(t)
	if _, recognized, err := evaluator.selectedToolSourceQueryRequest(KbuildActionRoleToken("target", "bindgen") + " --version"); err != nil || recognized {
		t.Fatalf("direct tool query recognized=%t err=%v, want unclaimed", recognized, err)
	}
}
