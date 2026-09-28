package kconfig

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestLinuxSourceScriptProbeDiscoversExtensionlessHelperFromShebang(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "compiler-capability"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	evaluator.sourceRoot = root
	request := singleSourceScriptRequest(t, evaluator,
		filepath.ToSlash(filepath.Join(root, "scripts", "compiler-capability"))+` /configured/clang`)
	if got, want := request.Sources, []string{"Kconfig", "scripts/compiler-capability"}; !slices.Equal(got, want) {
		t.Fatalf("extensionless probe sources=%q, want %q", got, want)
	}
	if got := request.Steps[0].AuxiliaryTools; !slices.Contains(got, "cc") {
		t.Fatalf("extensionless probe tool roles=%q, want configured cc binding", got)
	}
}

func singleSourceScriptRequest(t *testing.T, evaluator *LinuxProbeEvaluator, command string) ProbeRequest {
	t.Helper()
	value, err := evaluator.Shell(context.Background(), command)
	if err != nil {
		t.Fatalf("Shell(%q): %v", command, err)
	}
	if !linuxProbeSymbolPattern.MatchString(value) {
		t.Fatalf("Shell(%q) = %q, want symbolic result", command, value)
	}
	references := evaluator.References()
	if len(references) == 0 {
		t.Fatal("source script emitted no probe reference")
	}
	symbol := evaluator.symbols[value]
	return symbol.request
}

func TestLinuxSourceScriptProbeUsesDeclaredScriptRuntimeAndConfiguredToolProxy(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	request := singleSourceScriptRequest(t, evaluator,
		`{ /src/scripts/gcc-x86_32-has-stack-protector.sh /configured/clang -fintegrated-as; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	if got, want := request.Sources, []string{"Kconfig", "scripts/gcc-x86_32-has-stack-protector.sh"}; !slices.Equal(got, want) {
		t.Fatalf("sources = %q, want %q", got, want)
	}
	if got, want := request.SourceRoots, []string{"linux", "rust"}; !slices.Equal(got, want) {
		t.Fatalf("source roots = %q, want %q", got, want)
	}
	step := request.Steps[0]
	if step.Tool != "scriptrun" || step.WorkingDirectory != "${source_root:linux}" || !slices.Equal(step.AuxiliaryTools, []string{"bindgen", "cc", "rustc"}) {
		t.Fatalf("source script step = %#v", step)
	}
	joined := strings.Join(step.Arguments, " ")
	for _, required := range []string{
		"${tool:script-runtime}",
		"${source:scripts/gcc-x86_32-has-stack-protector.sh}",
		"cc=${tool:cc}",
		"-- cc -fintegrated-as",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("source script arguments %q omit %q", joined, required)
		}
	}
	// Compiler behavior belongs to the declared source file. The request must
	// not reconstruct either the historical bad %gs check or the fixed %fs
	// guard options in Go.
	for _, forbidden := range []string{"%gs", "%fs", "mstack-protector-guard"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("source script request hardcodes %q in %q", forbidden, joined)
		}
	}
	separator := slices.Index(step.Arguments, "--")
	if separator < 0 || !reflect.DeepEqual(step.Candidate, &ProbeCandidateArguments{
		Policy: ProbeCandidatePolicyCCLink,
		Base:   []int{separator + 2},
	}) {
		t.Fatalf("source script candidate ownership = %#v, argv=%q", step.Candidate, step.Arguments)
	}
}

func TestLinuxSourceScriptProbeTracksOnlyCompilerCandidatePositions(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)

	t.Run("tool proxy value remains position specific", func(t *testing.T) {
		request := singleSourceScriptRequest(t, evaluator,
			`/src/scripts/cc-can-link.sh /configured/clang cc -fsource-selected`)
		step := request.Steps[0]
		separator := slices.Index(step.Arguments, "--")
		if separator < 0 || !slices.Equal(step.Arguments[separator+1:], []string{"cc", "cc", "-fsource-selected"}) {
			t.Fatalf("source script trailing argv = %q", step.Arguments)
		}
		if !reflect.DeepEqual(step.Candidate, &ProbeCandidateArguments{
			Policy: ProbeCandidatePolicyCCLink,
			Base:   []int{separator + 2, separator + 3},
		}) {
			t.Fatalf("source script candidate ownership = %#v", step.Candidate)
		}
	})

	t.Run("fragmented candidate owns base slot", func(t *testing.T) {
		text, err := evaluator.requestText(ProbeRequest{
			Schema: LinuxProbeRequestSchema,
			Steps:  []ProbeStep{{Name: "flags", Tool: "cc", Arguments: []string{"--version"}}},
			Outcome: ProbeOutcome{
				Kind: "text", Step: "flags", Stream: "stdout", TrimSpace: true,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		request := singleSourceScriptRequest(t, evaluator,
			`/src/scripts/cc-can-link.sh /configured/clang `+text)
		step := request.Steps[0]
		separator := slices.Index(step.Arguments, "--")
		fragmentIndex := separator + 2
		if separator < 0 || len(step.ArgumentFragments) != 1 || step.ArgumentFragments[0].Index != fragmentIndex {
			t.Fatalf("source script fragmented argv = %#v, base=%q", step.ArgumentFragments, step.Arguments)
		}
		if !reflect.DeepEqual(step.Candidate, &ProbeCandidateArguments{
			Policy: ProbeCandidatePolicyCCLink,
			Base:   []int{fragmentIndex},
		}) {
			t.Fatalf("source script fragmented candidate ownership = %#v", step.Candidate)
		}
	})
}

func TestLinuxSourceScriptProbeInstallsToolchainRuntimeAppletOverrides(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	role := compactKbuildScriptAppletRolePrefix + "find"
	evaluator.tools[role] = "/configured/toybox"
	request := singleSourceScriptRequest(t, evaluator, `/src/scripts/cc-version.sh /configured/clang`)
	step := request.Steps[0]
	if !slices.Contains(step.AuxiliaryTools, role) {
		t.Fatalf("source script auxiliary roles=%q, want %q", step.AuxiliaryTools, role)
	}
	joined := strings.Join(step.Arguments, " ")
	if want := "-applet find=${tool:" + role + "}"; !strings.Contains(joined, want) {
		t.Fatalf("source script arguments=%q, want %q", joined, want)
	}
}

func TestLinuxSourceScriptProbeUsesScopedRoleTokenWhenToolPathsAlias(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	facts, err := ParseLinuxCompilerBootstrapResult(fixture.result, fixture.scope, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	const sharedDriver = "/configured/shared-driver"
	evaluator, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{
		Scope: fixture.scope, Architecture: "arm64", SourceArchitecture: "arm64", SourceRoot: "/src", Facts: facts,
		Tools: map[string]string{"as": sharedDriver, "cc": sharedDriver},
		ScriptEnvironment: map[string]string{
			"CC": KbuildActionRoleToken("target", "cc"),
		},
		Discovery: builder,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := singleSourceScriptRequest(t, evaluator,
		`/src/scripts/cc-version.sh `+KbuildActionRoleToken("target", "cc"))
	step := request.Steps[0]
	if got, want := step.AuxiliaryTools, []string{"cc"}; !slices.Equal(got, want) {
		t.Fatalf("aliased driver auxiliary roles = %q, want scoped role %q", got, want)
	}
	if got := strings.Join(step.Arguments, " "); !strings.Contains(got, "-- cc") {
		t.Fatalf("aliased driver was not rewritten through cc proxy: %q", got)
	}
	roleToken := KbuildActionRoleToken("target", "cc")
	if got, err := evaluator.Shell(context.Background(), "command -v "+roleToken); err != nil || got != roleToken {
		t.Fatalf("command -v scoped alias = %q, %v; want %q", got, err, roleToken)
	}
}

func TestLinuxSourceScriptProbeAllowsAbsentBareLeadingToolOnly(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 1, "x86", builder, nil, false)
	request := singleSourceScriptRequest(t, evaluator,
		`/src/scripts/rustc-version.sh rustc`)
	step := request.Steps[0]
	if slices.Contains(step.AuxiliaryTools, "rustc") {
		t.Fatalf("absent bare tool became a configured proxy: %q", step.AuxiliaryTools)
	}
	if got := step.Arguments[len(step.Arguments)-2:]; !slices.Equal(got, []string{"--", "rustc"}) {
		t.Fatalf("source script trailing argv = %q, want absent bare tool passed through", got)
	}
	if step.Candidate != nil {
		t.Fatalf("managed leading command was tagged as a compiler candidate: %#v", step.Candidate)
	}

	for _, command := range []string{
		`/src/scripts/rustc-version.sh rustc positional`,
		`/src/scripts/rustc-version.sh rustc ../input`,
	} {
		request := singleSourceScriptRequest(t, evaluator, command)
		step := request.Steps[0]
		separator := slices.Index(step.Arguments, "--")
		if separator < 0 || !reflect.DeepEqual(step.Candidate, &ProbeCandidateArguments{
			Policy: ProbeCandidatePolicyCCLink,
			Base:   []int{separator + 2},
		}) {
			t.Errorf("Shell(%q) candidate ownership = %#v, argv=%q", command, step.Candidate, step.Arguments)
		}
	}
}

func TestLinuxSourceScriptProbeAllowsConditionalBareLeadingTool(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 1, "x86", builder, nil, false)
	truth, err := evaluator.requestTruth(ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps:  []ProbeStep{{Name: "select", Tool: "cc", Arguments: []string{"--version"}}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{
			Operator: "exit-zero", Step: "select",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := evaluator.renderTruth(truth, "rustc", "")
	if err != nil {
		t.Fatal(err)
	}
	request := singleSourceScriptRequest(t, evaluator,
		`/src/scripts/tool-version.sh `+candidate)
	step := request.Steps[0]
	if len(step.ConditionalArguments) != 1 {
		t.Fatalf("conditional source script argv = %#v, want one leading candidate", step.ConditionalArguments)
	}
	conditional := step.ConditionalArguments[0]
	if conditional.Before != len(step.Arguments) ||
		conditional.When.Operator != "result-true" ||
		!slices.Equal(conditional.Arguments, []string{"rustc"}) {
		t.Fatalf("conditional source script argv = %#v, base=%q", conditional, step.Arguments)
	}
	if step.Candidate != nil {
		t.Fatalf("conditional leading command was tagged as a compiler candidate: %#v", step.Candidate)
	}
	unsafe, err := evaluator.renderTruth(truth, "rustc positional", "")
	if err != nil {
		t.Fatal(err)
	}
	command := `/src/scripts/tool-version.sh ` + unsafe
	unsafeRequest := singleSourceScriptRequest(t, evaluator, command)
	unsafeStep := unsafeRequest.Steps[0]
	if !reflect.DeepEqual(unsafeStep.Candidate, &ProbeCandidateArguments{
		Policy:      ProbeCandidatePolicyCCLink,
		Conditional: []int{0},
	}) {
		t.Fatalf("Shell(%q) candidate ownership = %#v, want conditional group 0", command, unsafeStep.Candidate)
	}
}

func TestLinuxSourceScriptProbeLowersSymbolicInheritedEnvironment(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 1, "x86", builder, nil, false)
	truth, err := evaluator.requestTruth(ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps:  []ProbeStep{{Name: "select", Tool: "cc", Arguments: []string{"--version"}}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{
			Operator: "exit-zero", Step: "select",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := evaluator.renderTruth(truth, "-fselected", "-ffallback")
	if err != nil {
		t.Fatal(err)
	}
	evaluator.scriptEnvironment["KBUILD_CFLAGS"] = "prefix " + selected + " suffix"
	request := singleSourceScriptRequest(t, evaluator, `/src/scripts/tool-version.sh rustc`)
	step := request.Steps[0]
	if _, exists := step.Environment["KBUILD_CFLAGS"]; exists {
		t.Fatalf("symbolic KBUILD_CFLAGS remained a literal environment value: %#v", step.Environment)
	}
	index := slices.IndexFunc(step.EnvironmentFragments, func(entry ProbeEnvironmentFragments) bool {
		return entry.Name == "KBUILD_CFLAGS"
	})
	if index < 0 || len(step.EnvironmentFragments[index].Fragments) != 4 || request.InputCount != 1 {
		t.Fatalf("symbolic KBUILD_CFLAGS lowering: fragments=%#v input_count=%d", step.EnvironmentFragments, request.InputCount)
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), selected) || strings.Contains(string(data), linuxProbeSymbolPrefix) {
		t.Fatalf("planner-only environment atom leaked into source-script request: %s", data)
	}
}

func TestLinuxSourceScriptProbeLowersCanonicalWholeMakeTextEnvironmentExactly(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 1, "x86", builder, nil, false)
	truth, err := evaluator.requestTruth(ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps:  []ProbeStep{{Name: "select", Tool: "cc", Arguments: []string{"--version"}}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{
			Operator: "exit-zero", Step: "select",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	optional, err := evaluator.renderTruth(truth, "-foptional", "")
	if err != nil {
		t.Fatal(err)
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{"target": evaluator}}
	filtered, recognized, err := scopes.transformSymbolic("filter", []string{
		"-m32 -m64 --target=%",
		" -D__KERNEL__ " + optional + " --target=x86_64-linux-gnu  -m64 ",
	})
	if err != nil || !recognized {
		t.Fatalf("KBUILD_USERCFLAGS filter = %q, recognized=%v, %v", filtered, recognized, err)
	}
	_, filteredSymbol, ok := scopes.symbolOwner(filtered)
	if !ok || filteredSymbol.kind != "make-text" || filteredSymbol.makeText == nil ||
		filteredSymbol.makeText.protocolMode != linuxProbeMakeTextProtocolCanonicalWords ||
		filteredSymbol.makeText.protocolValue != "--target=x86_64-linux-gnu -m64" {
		t.Fatalf("KBUILD_USERCFLAGS filter protocol = %#v", filteredSymbol)
	}
	// Retain another canonical word expression around the filter. Exact process
	// lowering must flatten its nested argv protocol into one aggregate, not
	// normalize the surrounding assignment whitespace.
	nested, err := evaluator.renderMakeText(
		"strip", []string{filtered}, filtered, linuxProbeMakeTextProtocolCanonicalWords,
	)
	if err != nil {
		t.Fatal(err)
	}
	evaluator.scriptEnvironment["KBUILD_USERCFLAGS"] = "leading  " + nested + "  trailing"
	request := singleSourceScriptRequest(t, evaluator, `/src/scripts/tool-version.sh rustc`)
	step := request.Steps[0]
	entryIndex := slices.IndexFunc(step.EnvironmentFragments, func(entry ProbeEnvironmentFragments) bool {
		return entry.Name == "KBUILD_USERCFLAGS"
	})
	if entryIndex < 0 {
		t.Fatalf("KBUILD_USERCFLAGS environment fragments = %#v", step.EnvironmentFragments)
	}
	fragments := step.EnvironmentFragments[entryIndex].Fragments
	if len(fragments) != 3 || fragments[0].Value != "leading  " || fragments[2].Value != "  trailing" {
		t.Fatalf("KBUILD_USERCFLAGS surrounding fragments = %#v", fragments)
	}
	aggregate := fragments[1]
	if aggregate.Value != "" || len(aggregate.Fragments) != 1 ||
		aggregate.Fragments[0].Value != "--target=x86_64-linux-gnu -m64" ||
		len(aggregate.Transforms) != 1 || aggregate.Transforms[0].Function != "strip" {
		t.Fatalf("KBUILD_USERCFLAGS aggregate = %#v", aggregate)
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if linuxProbeSymbolPattern.Match(data) {
		t.Fatalf("KBUILD_USERCFLAGS request leaked planner token: %s", data)
	}

	for _, test := range []struct {
		name     string
		function string
		args     []string
	}{
		{name: "basename", function: "basename", args: []string{filtered}},
		{name: "notdir", function: "notdir", args: []string{filtered}},
		{name: "subst", function: "subst", args: []string{"x", "y", filtered}},
		{name: "addprefix", function: "addprefix", args: []string{"prefix", filtered}},
		{name: "addsuffix", function: "addsuffix", args: []string{"suffix", filtered}},
		{name: "patsubst", function: "patsubst", args: []string{"%", "x%", filtered}},
		{name: "wordlist", function: "wordlist", args: []string{"1", "2", filtered}},
	} {
		t.Run(test.name+" remains argv-only", func(t *testing.T) {
			value, recognized, err := scopes.transformSymbolic(test.function, test.args)
			if err != nil || !recognized {
				t.Fatalf("%s = %q, recognized=%v, %v", test.function, value, recognized, err)
			}
			_, symbol, ok := scopes.symbolOwner(value)
			if !ok || symbol.makeText == nil || symbol.makeText.protocolMode != linuxProbeMakeTextProtocolArgvWords {
				t.Fatalf("%s protocol = %#v, want argv-only", test.function, symbol)
			}
			lowerer := newProbeSymbolicValueLowerer(evaluator)
			if _, _, err := lowerer.value(value); err == nil || !strings.Contains(err.Error(), "argv-word-equivalent but not exact") {
				t.Fatalf("%s exact lowering error = %v", test.function, err)
			}
		})
	}
}

func TestLinuxSourceScriptProbePreservesShellDollarProvenance(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 1, "x86", builder, nil, false)

	for _, command := range []string{
		`/src/scripts/tool-version.sh $CC`,
		`/src/scripts/tool-version.sh "$CC"`,
	} {
		request := singleSourceScriptRequest(t, evaluator, command)
		step := request.Steps[0]
		if got := step.Arguments[len(step.Arguments)-2:]; !slices.Equal(got, []string{"--", "cc"}) {
			t.Errorf("Shell(%q) trailing argv = %q, want selected cc proxy", command, got)
		}
	}
	for _, command := range []string{
		`/src/scripts/tool-version.sh --tag='$CC'`,
		`/src/scripts/tool-version.sh --tag=\$CC`,
	} {
		request := singleSourceScriptRequest(t, evaluator, command)
		step := request.Steps[0]
		got := step.Arguments[len(step.Arguments)-1]
		if got != `--tag=$CC` {
			t.Errorf("Shell(%q) literal-dollar argv = %q, want %q", command, got, `--tag=$CC`)
		}
		if strings.Contains(got, compactKbuildLiteralDollarToken) {
			t.Errorf("Shell(%q) leaked lexer sentinel in %q", command, got)
		}
		if !reflect.DeepEqual(step.Candidate, &ProbeCandidateArguments{
			Policy: ProbeCandidatePolicyCCLink,
			Base:   []int{len(step.Arguments) - 1},
		}) {
			t.Errorf("Shell(%q) candidate ownership = %#v", command, step.Candidate)
		}
	}
	literal := singleSourceScriptRequest(t, evaluator, `/src/scripts/tool-version.sh '$CC'`)
	literalStep := literal.Steps[0]
	if got := literalStep.Arguments[len(literalStep.Arguments)-1]; got != "$CC" || strings.Contains(got, compactKbuildLiteralDollarToken) {
		t.Fatalf("literal-dollar arg0 = %q, want exact $CC", got)
	}
	if !reflect.DeepEqual(literalStep.Candidate, &ProbeCandidateArguments{
		Policy: ProbeCandidatePolicyCCLink,
		Base:   []int{len(literalStep.Arguments) - 1},
	}) {
		t.Fatalf("literal-dollar arg0 candidate ownership = %#v", literalStep.Candidate)
	}
}

func TestLinuxSourceScriptProbeExpandsExactEnvironmentAssignmentsOnly(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 1, "x86", builder, nil, false)

	active := singleSourceScriptRequest(t, evaluator,
		`env TOOL=$CC ARCHITECTURE=${ARCH} /src/scripts/tool-version.sh`)
	if got, want := active.Steps[0].Environment["TOOL"], "cc"; got != want {
		t.Fatalf("active TOOL = %q, want configured proxy %q", got, want)
	}
	if got, want := active.Steps[0].Environment["ARCHITECTURE"], "x86"; got != want {
		t.Fatalf("active ARCHITECTURE = %q, want exact environment value %q", got, want)
	}
	for _, command := range []string{
		`env 'TOOL=$CC' /src/scripts/tool-version.sh`,
		`env TOOL=\$CC /src/scripts/tool-version.sh`,
	} {
		request := singleSourceScriptRequest(t, evaluator, command)
		if got := request.Steps[0].Environment["TOOL"]; got != "$CC" {
			t.Errorf("Shell(%q) literal TOOL = %q, want literal $CC", command, got)
		} else if strings.Contains(got, compactKbuildLiteralDollarToken) {
			t.Errorf("Shell(%q) leaked lexer sentinel in environment %q", command, got)
		}
	}
}

func TestLinuxSourceScriptProbeRejectsUnmodeledShellExpansionAndPlaceholderCollisions(t *testing.T) {
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	evaluator, _ := newFixtureProbeEvaluator(t, 1, "x86", builder, nil, false)
	evaluator.scriptEnvironment["EMPTY"] = ""
	evaluator.scriptEnvironment["FLAGS"] = "-first -second"
	evaluator.scriptEnvironment["PLACEHOLDER"] = "${tool:cc}"
	for _, command := range []string{
		`/src/scripts/tool-version.sh --driver=$CC`,
		`env TOOL=$CC-suffix /src/scripts/tool-version.sh`,
		"/src/scripts/tool-version.sh --tag=`ambient`",
		`/src/scripts/tool-version.sh --tag='${tool:cc}'`,
		`env TAG='${source:Kconfig}' /src/scripts/tool-version.sh`,
		`/src/scripts/tool-version.sh $EMPTY`,
		`/src/scripts/tool-version.sh $FLAGS`,
		`env TAG=$EMPTY /src/scripts/tool-version.sh`,
		`env TAG=$FLAGS /src/scripts/tool-version.sh`,
		`env TAG=$PLACEHOLDER /src/scripts/tool-version.sh`,
	} {
		if _, err := evaluator.Shell(context.Background(), command); err == nil || IsLinuxProbeUnsupportedCommand(err) {
			t.Errorf("Shell(%q) error = %v, want owned fail-closed rejection", command, err)
		}
	}
	if len(evaluator.References()) != 0 {
		t.Fatalf("rejected shell expansions emitted %d probe references", len(evaluator.References()))
	}
}

func TestLinuxSourceScriptProbeLowersEnvToolBindingsWithoutScriptNameTable(t *testing.T) {
	builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	request := singleSourceScriptRequest(t, evaluator,
		`{ env "CC=/configured/clang" "LD=/configured/ld.lld" "NM=/configured/llvm-nm" "OBJCOPY=/configured/llvm-objcopy" /src/scripts/tools-support-relr.sh; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	step := request.Steps[0]
	if got, want := step.AuxiliaryTools, []string{"bindgen", "cc", "ld", "nm", "objcopy", "rustc"}; !slices.Equal(got, want) {
		// The test evaluator models the same inherited tool environment supplied
		// to Kconfig, so all inherited selected tools remain exact proxies.
		t.Fatalf("auxiliary tools = %q, want %q", got, want)
	}
	for name, want := range map[string]string{"CC": "cc", "LD": "ld", "NM": "nm", "OBJCOPY": "objcopy"} {
		if got := step.Environment[name]; got != want {
			t.Errorf("environment %s = %q, want %q", name, got, want)
		}
	}
	if request.Sources[1] != "scripts/tools-support-relr.sh" {
		t.Fatalf("generic source path = %q", request.Sources)
	}
}

func TestLinuxRustAvailabilityScriptUsesSelectedRustSourceRoot(t *testing.T) {
	builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	request := singleSourceScriptRequest(t, evaluator,
		`{ /src/scripts/rust_is_available.sh; } >/dev/null 2>&1 && echo "y" || echo "n"`)
	if got, want := request.SourceRoots, []string{"linux", "rust"}; !slices.Equal(got, want) {
		t.Fatalf("source roots = %q, want %q", got, want)
	}
	step := request.Steps[0]
	if got := step.Environment["RUST_LIB_SRC"]; got != "${source_root:rust}" {
		t.Fatalf("RUST_LIB_SRC = %q", got)
	}
	for _, role := range []string{"bindgen", "cc", "rustc"} {
		if !slices.Contains(step.AuxiliaryTools, role) {
			t.Errorf("Rust availability does not proxy %s: %q", role, step.AuxiliaryTools)
		}
	}
}

func TestLinuxSourceScriptPathChangesCanonicalRequest(t *testing.T) {
	builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	first := singleSourceScriptRequest(t, evaluator,
		`/src/scripts/cc-version.sh /configured/clang`)
	second := singleSourceScriptRequest(t, evaluator,
		`/src/scripts/as-version.sh /configured/clang -fintegrated-as`)
	firstID, err := first.ID()
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := second.ID()
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID {
		t.Fatalf("distinct declared source scripts share request ID %s", firstID)
	}
}

func TestLinuxSourceScriptTextWordProjectionDiscoversAndReplaysDependency(t *testing.T) {
	const scriptCommand = `/src/scripts/cc-version.sh /configured/clang`

	discoveryBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	discovery, _ := testSymbolicProbeEvaluator(t, discoveryBuilder, nil)
	scriptValue, err := discovery.Shell(context.Background(), scriptCommand)
	if err != nil {
		t.Fatal(err)
	}
	wordValue, err := discovery.Shell(context.Background(), "set -- "+scriptValue+" && echo $2")
	if err != nil {
		t.Fatal(err)
	}
	if !linuxProbeSymbolPattern.MatchString(scriptValue) || !linuxProbeSymbolPattern.MatchString(wordValue) || scriptValue == wordValue {
		t.Fatalf("discovery values = script %q, word %q; want distinct symbolic text nodes", scriptValue, wordValue)
	}

	wordSymbol := discovery.symbols[wordValue]
	plan, err := discoveryBuilder.Plan(wordSymbol.reference)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 2 {
		t.Fatalf("word projection plan nodes = %#v, want source and dependent reduction", plan.Nodes)
	}
	scriptNode, wordNode := plan.Nodes[0], plan.Nodes[1]
	if got, want := wordNode.Inputs, []string{scriptNode.ID}; !slices.Equal(got, want) {
		t.Fatalf("word projection inputs = %q, want %q", got, want)
	}
	wordRequest := plan.Requests[wordNode.RequestID]
	if wordRequest.InputCount != 1 || len(wordRequest.Steps) != 0 || wordRequest.Outcome.Kind != "text" || wordRequest.Outcome.Result != "00000000" || wordRequest.Outcome.Word != 2 {
		t.Fatalf("word projection request = %#v", wordRequest)
	}
	if got := wordRequest.ToolRoles(); len(got) != 0 {
		t.Fatalf("word projection tool roles = %q, want none", got)
	}

	results := probeResultMap{
		scriptNode.ID: {
			Schema: LinuxProbeResultSchema, NodeID: scriptNode.ID, RequestID: scriptNode.RequestID,
			Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: "Clang 220108",
			Steps: []ProbeStepResult{{Name: "source-script", Status: "success", ExitCode: 0, Stdout: "Clang 220108\n"}},
		},
		wordNode.ID: {
			Schema: LinuxProbeResultSchema, NodeID: wordNode.ID, RequestID: wordNode.RequestID,
			Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: "220108",
		},
	}
	replayBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	replay, _ := testSymbolicProbeEvaluator(t, replayBuilder, results)
	replayScriptValue, err := replay.Shell(context.Background(), scriptCommand)
	if err != nil {
		t.Fatal(err)
	}
	replayWordValue, err := replay.Shell(context.Background(), "set -- "+replayScriptValue+" && echo $2")
	if err != nil {
		t.Fatal(err)
	}
	if replayScriptValue != scriptValue || replayWordValue != wordValue {
		t.Fatalf("replay symbolic values = script %q, word %q; want discovery %q, %q", replayScriptValue, replayWordValue, scriptValue, wordValue)
	}
	if got, err := replay.ResolveSymbolic(replayWordValue); err != nil || got != "220108" {
		t.Fatalf("ResolveSymbolic(word) = %q, %v; want 220108", got, err)
	}
	replayPlan, err := replayBuilder.Plan(replay.symbols[replayWordValue].reference)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayPlan.Nodes) != 2 || replayPlan.Nodes[0].ID != scriptNode.ID || replayPlan.Nodes[1].ID != wordNode.ID || !slices.Equal(replayPlan.Nodes[1].Inputs, wordNode.Inputs) {
		t.Fatalf("replay dependency DAG = %#v, want %#v", replayPlan.Nodes, plan.Nodes)
	}
}

func TestLinuxSourceScriptProbeRejectsShellComposition(t *testing.T) {
	builder, _ := NewProbePlanBuilder(bootstrapTestIdentity, "")
	evaluator, _ := testSymbolicProbeEvaluator(t, builder, nil)
	if _, err := evaluator.Shell(context.Background(), `/src/scripts/cc-version.sh /configured/clang | /tmp/ambient`); err == nil || IsLinuxProbeUnsupportedCommand(err) {
		t.Fatalf("composed source script error = %v, want owned failure", err)
	}
}

func testSourceScriptOutputScopes(
	t *testing.T,
	root string,
	discovery ProbeDiscovery,
	oracle ProbeResultLookup,
) *KbuildProbeScopes {
	t.Helper()
	fixture := linuxCompilerBootstrapFixtures(t)[1]
	facts, err := ParseLinuxCompilerBootstrapResult(fixture.result, fixture.scope, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]string{
		"cc":                    "/configured/target/cc",
		linuxProbeScriptRunner:  "/configured/scriptrun",
		linuxProbeScriptRuntime: "/configured/script-runtime",
		compactKbuildScriptAppletRolePrefix + "cat": "/configured/cat",
	}
	evaluator, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{
		Scope: fixture.scope, Architecture: "x86", SourceArchitecture: "x86", SourceRoot: root,
		Facts: facts, Tools: tools,
		ScriptEnvironment: map[string]string{"CC": tools["cc"], "SCRIPT_MODE": "exact"},
		Discovery:         discovery, Oracle: oracle,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{fixture.scope: evaluator}}
}

func testSourceScriptOutputDualScopes(
	t *testing.T,
	root string,
	discovery ProbeDiscovery,
	oracle ProbeResultLookup,
) *KbuildProbeScopes {
	t.Helper()
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{}}
	registry := newLinuxProbeSymbolRegistry()
	fixtures := linuxCompilerBootstrapFixtures(t)
	for _, fixture := range []bootstrapFixture{fixtures[1], fixtures[0]} {
		facts, err := ParseLinuxCompilerBootstrapResult(fixture.result, fixture.scope, bootstrapTestIdentity)
		if err != nil {
			t.Fatal(err)
		}
		tools := map[string]string{
			"cc":                    "/configured/" + fixture.scope + "/cc",
			linuxProbeScriptRunner:  "/configured/" + fixture.scope + "/scriptrun",
			linuxProbeScriptRuntime: "/configured/" + fixture.scope + "/script-runtime",
			compactKbuildScriptAppletRolePrefix + "cat": "/configured/" + fixture.scope + "/cat",
		}
		evaluator, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{
			Scope: fixture.scope, Architecture: "x86", SourceArchitecture: "x86", SourceRoot: root,
			Facts: facts, Tools: tools,
			ScriptEnvironment: map[string]string{"CC": tools["cc"], "SCRIPT_MODE": fixture.scope},
			Discovery:         discovery, Oracle: oracle,
		})
		if err != nil {
			t.Fatal(err)
		}
		evaluator.symbolRegistry = registry
		scopes.evaluators[fixture.scope] = evaluator
	}
	return scopes
}

func sourceScriptOutputFixture(t *testing.T) (string, []KbuildSourceScriptArgument) {
	t.Helper()
	root := t.TempDir()
	mustWriteSource(t, root, linuxProbeRootAnchor, "mainmenu \"fixture\"\n")
	mustWriteSource(t, root, "scripts/render.sh", "#!/bin/sh\nprintf '%s\\n' \"$3\" >\"$1\"\ncat \"$2\" >>\"$1\"\n\"$4\" --version >/dev/null\n")
	mustWriteSource(t, root, "scripts/emit.sh", "#!/bin/sh\nprintf '%s\\n' \"$1\"\ncat \"$2\"\n\"$3\" --version >/dev/null\n")
	mustWriteSource(t, root, "data/input.txt", "payload\n")
	return root, []KbuildSourceScriptArgument{
		{Kind: KbuildSourceScriptOutputArgument},
		{Kind: KbuildSourceScriptSourceArgument, Value: "data/input.txt"},
		{Kind: KbuildSourceScriptLiteralArgument, Value: "literal value"},
		{Kind: KbuildSourceScriptToolArgument, Value: "cc"},
	}
}

func sourceScriptStdoutArguments() []KbuildSourceScriptArgument {
	return []KbuildSourceScriptArgument{
		{Kind: KbuildSourceScriptStdoutArgument},
		{Kind: KbuildSourceScriptLiteralArgument, Value: "stdout literal"},
		{Kind: KbuildSourceScriptSourceArgument, Value: "data/input.txt"},
		{Kind: KbuildSourceScriptToolArgument, Value: "cc"},
	}
}

func TestKbuildSourceScriptOutputTextDiscoveryUsesTypedTwoStepProtocol(t *testing.T) {
	root, arguments := sourceScriptOutputFixture(t)
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	scopes := testSourceScriptOutputScopes(t, root, builder, nil)
	text, concrete, err := scopes.SourceScriptOutputText("target", "scripts/render.sh", arguments)
	if err != nil {
		t.Fatal(err)
	}
	if text != "" || concrete {
		t.Fatalf("discovery result = (%q, %t), want empty, non-concrete", text, concrete)
	}
	plan, err := builder.Plan(scopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 || !slices.Equal(plan.Terminal, []string{plan.Nodes[0].ID}) {
		t.Fatalf("source-script output plan = %#v", plan)
	}
	request := plan.Requests[plan.Nodes[0].RequestID]
	if got, want := request.Sources, []string{"Kconfig", "data/input.txt", "scripts/render.sh"}; !slices.Equal(got, want) {
		t.Fatalf("sources = %q, want %q", got, want)
	}
	if got, want := request.SourceRoots, []string{"linux"}; !slices.Equal(got, want) {
		t.Fatalf("source roots = %q, want %q", got, want)
	}
	if len(request.Scratch) != 1 || request.Scratch[0] != (ProbeScratch{Name: linuxProbeScriptOutput, Kind: "file"}) {
		t.Fatalf("scratch = %#v", request.Scratch)
	}
	if len(request.Steps) != 2 || request.Steps[0].Name != "source-script-output" || request.Steps[1].Name != "read-source-script-output" {
		t.Fatalf("steps = %#v", request.Steps)
	}
	generate, read := request.Steps[0], request.Steps[1]
	if !generate.DiscardStdout || generate.DiscardStderr || generate.WorkingDirectory != "${source_root:linux}" {
		t.Fatalf("generate step authority = %#v", generate)
	}
	joined := strings.Join(generate.Arguments, "\x00")
	for _, want := range []string{
		"${source:scripts/render.sh}",
		"cc=${tool:cc}",
		"${scratch:output}",
		"${source:data/input.txt}",
		"literal value",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("generate argv %q omits %q", generate.Arguments, want)
		}
	}
	if got := generate.Environment["CC"]; got != "cc" {
		t.Fatalf("configured CC environment = %q, want private proxy name", got)
	}
	if got, want := generate.AuxiliaryTools, []string{"cc", compactKbuildScriptAppletRolePrefix + "cat"}; !slices.Equal(got, want) {
		t.Fatalf("generate auxiliary tools = %q, want %q", got, want)
	}
	encodedIndex := slices.Index(read.Arguments, "-script_content_base64")
	if encodedIndex < 0 || encodedIndex+1 >= len(read.Arguments) {
		t.Fatalf("reader has no encoded script: %q", read.Arguments)
	}
	decoded, err := base64.StdEncoding.DecodeString(read.Arguments[encodedIndex+1])
	if err != nil || string(decoded) != linuxProbeReadOutputScript {
		t.Fatalf("reader script = %q, %v; want %q", decoded, err, linuxProbeReadOutputScript)
	}
	if !slices.Contains(read.Arguments, "cat") || !slices.Contains(read.Arguments, "${scratch:output}") {
		t.Fatalf("reader argv = %q", read.Arguments)
	}
	if !reflect.DeepEqual(request.Outcome, ProbeOutcome{Kind: "text", Step: "read-source-script-output", Stream: "stdout", RequireSuccess: true}) {
		t.Fatalf("outcome = %#v", request.Outcome)
	}
	data, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{root, "/configured/target/cc", "/configured/scriptrun", "/configured/script-runtime"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("canonical request contains executor-specific spelling %q: %s", forbidden, data)
		}
	}
}

func TestKbuildSourceScriptOutputTextReplayReturnsExactFileBytes(t *testing.T) {
	root, arguments := sourceScriptOutputFixture(t)
	discoveryBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	discoveryScopes := testSourceScriptOutputScopes(t, root, discoveryBuilder, nil)
	if _, concrete, err := discoveryScopes.SourceScriptOutputText("target", "scripts/render.sh", arguments); err != nil || concrete {
		t.Fatalf("discovery = concrete %t, %v", concrete, err)
	}
	discoveryPlan, err := discoveryBuilder.Plan(discoveryScopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	node := discoveryPlan.Nodes[0]
	const exact = "literal value\npayload\n\n"
	result := ProbeResult{
		Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
		Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: exact,
		Steps: []ProbeStepResult{
			{Name: "source-script-output", Status: "success", ExitCode: 0},
			{Name: "read-source-script-output", Status: "success", ExitCode: 0, Stdout: exact},
		},
	}
	replayBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	replayScopes := testSourceScriptOutputScopes(t, root, replayBuilder, probeResultMap{node.ID: result})
	got, concrete, err := replayScopes.SourceScriptOutputText("target", "scripts/render.sh", arguments)
	if err != nil {
		t.Fatal(err)
	}
	if !concrete || got != exact {
		t.Fatalf("replay result = (%q, %t), want exact %q", got, concrete, exact)
	}
	replayPlan, err := replayBuilder.Plan(replayScopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayPlan.Nodes) != 1 || replayPlan.Nodes[0].ID != node.ID || replayPlan.Nodes[0].RequestID != node.RequestID ||
		!slices.Equal(replayPlan.Nodes[0].Inputs, node.Inputs) {
		t.Fatalf("replay node = %#v, want %#v", replayPlan.Nodes, node)
	}
}

func TestKbuildSourceScriptOutputTextDiscoveryUsesExactStdoutAuthority(t *testing.T) {
	root, _ := sourceScriptOutputFixture(t)
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	scopes := testSourceScriptOutputScopes(t, root, builder, nil)
	text, concrete, err := scopes.SourceScriptOutputText("target", "scripts/emit.sh", sourceScriptStdoutArguments())
	if err != nil {
		t.Fatal(err)
	}
	if text != "" || concrete {
		t.Fatalf("stdout discovery result = (%q, %t), want empty, non-concrete", text, concrete)
	}
	plan, err := builder.Plan(scopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 {
		t.Fatalf("stdout discovery plan = %#v", plan.Nodes)
	}
	request := plan.Requests[plan.Nodes[0].RequestID]
	if len(request.Scratch) != 0 || len(request.Steps) != 1 {
		t.Fatalf("stdout request scratch/steps = %#v / %#v", request.Scratch, request.Steps)
	}
	step := request.Steps[0]
	if step.Name != "source-script-output" || step.DiscardStdout || step.DiscardStderr {
		t.Fatalf("stdout generator streams = %#v", step)
	}
	separator := slices.Index(step.Arguments, "--")
	if separator < 0 {
		t.Fatalf("stdout generator has no argv separator: %q", step.Arguments)
	}
	if got, want := step.Arguments[separator+1:], []string{"stdout literal", "${source:data/input.txt}", "cc"}; !slices.Equal(got, want) {
		t.Fatalf("stdout script argv = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(step.Arguments, "\x00"), "${scratch:output}") {
		t.Fatalf("stdout authority leaked a scratch argv: %q", step.Arguments)
	}
	if got, want := request.Outcome, (ProbeOutcome{Kind: "text", Step: "source-script-output", Stream: "stdout", RequireSuccess: true}); !reflect.DeepEqual(got, want) {
		t.Fatalf("stdout outcome = %#v, want %#v", got, want)
	}
}

func TestKbuildSourceScriptOutputTextReplayReturnsExactStdoutBytes(t *testing.T) {
	root, _ := sourceScriptOutputFixture(t)
	arguments := sourceScriptStdoutArguments()
	discoveryBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	discoveryScopes := testSourceScriptOutputScopes(t, root, discoveryBuilder, nil)
	if _, concrete, err := discoveryScopes.SourceScriptOutputText("target", "scripts/emit.sh", arguments); err != nil || concrete {
		t.Fatalf("stdout discovery = concrete %t, %v", concrete, err)
	}
	discoveryPlan, err := discoveryBuilder.Plan(discoveryScopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	node := discoveryPlan.Nodes[0]
	const exact = "stdout literal\npayload\n\n"
	result := ProbeResult{
		Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
		Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: exact,
		Steps: []ProbeStepResult{{
			Name: "source-script-output", Status: "success", ExitCode: 0,
			Stdout: exact, Stderr: "source-owned diagnostic\n",
		}},
	}
	replayBuilder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	replayScopes := testSourceScriptOutputScopes(t, root, replayBuilder, probeResultMap{node.ID: result})
	got, concrete, err := replayScopes.SourceScriptOutputText("target", "scripts/emit.sh", arguments)
	if err != nil {
		t.Fatal(err)
	}
	if !concrete || got != exact {
		t.Fatalf("stdout replay result = (%q, %t), want exact %q", got, concrete, exact)
	}
	replayPlan, err := replayBuilder.Plan(replayScopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayPlan.Nodes) != 1 || replayPlan.Nodes[0].ID != node.ID {
		t.Fatalf("stdout replay nodes = %#v, want %s", replayPlan.Nodes, node.ID)
	}
}

func evaluatedScriptOutputRecipeForTest() (string, string) {
	target := "include/generated/timeconst.h"
	return target, "{ echo 250 | bc -q ${tree:kernel}/kernel/time/timeconst.bc; } > " + target
}

func TestKbuildEvaluatedScriptOutputTextDiscoveryUsesExactRecipeAndEnvironment(t *testing.T) {
	root, _ := sourceScriptOutputFixture(t)
	mustWriteSource(t, root, "kernel/time/timeconst.bc", "scale=6\nprint HZ\n")
	builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	scopes := testSourceScriptOutputScopes(t, root, builder, nil)
	target, recipe := evaluatedScriptOutputRecipeForTest()
	text, concrete, recognized, err := scopes.EvaluatedScriptOutputText(
		"target", target, recipe, []string{"kernel/time/timeconst.bc"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if text != "" || concrete || !recognized {
		t.Fatalf("discovery result = (%q,%t,%t), want empty, pending, recognized", text, concrete, recognized)
	}
	plan, err := builder.Plan(scopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 1 {
		t.Fatalf("evaluated-script probe nodes = %#v", plan.Nodes)
	}
	request := plan.Requests[plan.Nodes[0].RequestID]
	if got, want := request.Sources, []string{"Kconfig", "kernel/time/timeconst.bc"}; !slices.Equal(got, want) {
		t.Fatalf("sources = %q, want %q", got, want)
	}
	if got, want := request.SourceRoots, []string{"linux"}; !slices.Equal(got, want) {
		t.Fatalf("source roots = %q, want %q", got, want)
	}
	if got, want := len(request.Steps), 3; got != want ||
		request.Steps[0].Name != "prepare-evaluated-script-output" ||
		request.Steps[1].Name != "evaluated-script-output" ||
		request.Steps[2].Name != "validate-evaluated-script-output" {
		t.Fatalf("steps = %#v", request.Steps)
	}
	prepare, step, validate := request.Steps[0], request.Steps[1], request.Steps[2]
	if step.WorkingDirectory != "${scratch:working-tree}" ||
		prepare.WorkingDirectory != step.WorkingDirectory || validate.WorkingDirectory != step.WorkingDirectory ||
		step.Environment["SCRIPT_MODE"] != "exact" || step.Environment["CC"] != "cc" {
		t.Fatalf("evaluated-script execution contract = %#v", step)
	}
	encodedIndex := slices.Index(step.Arguments, "-script_content_base64")
	if encodedIndex < 0 || encodedIndex+1 >= len(step.Arguments) {
		t.Fatalf("evaluated-script argv has no encoded body: %q", step.Arguments)
	}
	decoded, err := base64.StdEncoding.DecodeString(step.Arguments[encodedIndex+1])
	if err != nil {
		t.Fatal(err)
	}
	wantMeasuredRecipe := "#!/bin/sh\nset -e\n" + strings.ReplaceAll(
		recipe, "${tree:kernel}/kernel/time/timeconst.bc", "kernel/time/timeconst.bc",
	) + "\n"
	if got := string(decoded); got != wantMeasuredRecipe {
		t.Fatalf("measured recipe = %q, want exact final shell %q", got, wantMeasuredRecipe)
	}
	if strings.Contains(string(decoded), "unsafe()") || strings.Contains(string(decoded), "probe_root") ||
		strings.Contains(string(decoded), "source_root") {
		t.Fatalf("measured recipe contains probe-only shell state: %q", decoded)
	}
	validationIndex := slices.Index(validate.Arguments, "-script_content_base64")
	if validationIndex < 0 || validationIndex+1 >= len(validate.Arguments) {
		t.Fatalf("validation argv has no encoded body: %q", validate.Arguments)
	}
	validationBody, err := base64.StdEncoding.DecodeString(validate.Arguments[validationIndex+1])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"find . -mindepth 1 ! -path './include/generated/timeconst.h' ! -path './include/generated' ! -path './include'",
		"if [ ! -f 'include/generated/timeconst.h' ] || [ -L 'include/generated/timeconst.h' ]; then unsafe; fi",
		"! -perm 0644",
		"if [ \"$size\" -gt 46080 ]; then unsafe; fi",
		"contains_transient()",
		linuxProbeEvaluatedScriptSafePrefix[:len(linuxProbeEvaluatedScriptSafePrefix)-1],
	} {
		if !strings.Contains(string(validationBody), want) {
			t.Errorf("validation script %q omits %q", validationBody, want)
		}
	}
	for _, want := range []string{"-timeout_seconds", "10", "-fallback_stdout_base64", "-max_file_size_bytes", "50176"} {
		if !slices.Contains(step.Arguments, want) {
			t.Errorf("measured recipe argv %q omits %q", step.Arguments, want)
		}
	}
	for _, want := range []string{"base64", "find", "grep", "sh", "wc", "kernel=${source_root:linux}", "cc=${tool:cc}"} {
		if !slices.Contains(validate.Arguments, want) {
			t.Errorf("validation argv %q omits %q", validate.Arguments, want)
		}
	}
	for _, want := range []string{"chmod", "cp", "mkdir", "sh", "kernel=${source_root:linux}"} {
		if !slices.Contains(prepare.Arguments, want) {
			t.Errorf("setup argv %q omits %q", prepare.Arguments, want)
		}
	}
	if slices.Contains(step.Arguments, "timeout") {
		t.Errorf("probe argv unexpectedly requires an inner-shell timeout applet: %q", step.Arguments)
	}
	if !slices.Contains(step.AuxiliaryTools, "cc") {
		t.Errorf("evaluated-script auxiliary tools %q omit inherited CC role", step.AuxiliaryTools)
	}
}

func TestKbuildEvaluatedScriptOutputTextCarriesExactWorkingTreeContents(t *testing.T) {
	root, _ := sourceScriptOutputFixture(t)
	mustWriteSource(t, root, "kernel/time/timeconst.bc", "fixture\n")
	target, recipe := evaluatedScriptOutputRecipeForTest()
	requestFor := func(content string) (ProbeRequest, ProbePlanNode) {
		t.Helper()
		builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
		if err != nil {
			t.Fatal(err)
		}
		scopes := testSourceScriptOutputScopes(t, root, builder, nil)
		if _, concrete, recognized, err := scopes.EvaluatedScriptOutputText(
			"target", target, recipe, []string{"kernel/time/timeconst.bc"},
			map[string]string{".config": content},
		); err != nil || concrete || !recognized {
			t.Fatalf("working-tree discovery = concrete %t, recognized %t, error %v", concrete, recognized, err)
		}
		plan, err := builder.Plan(scopes.References()...)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Nodes) != 1 {
			t.Fatalf("working-tree probe nodes = %#v", plan.Nodes)
		}
		node := plan.Nodes[0]
		return plan.Requests[node.RequestID], node
	}

	empty, emptyNode := requestFor("")
	if len(empty.Scratch) != 2 || empty.Scratch[0].Name != "working-input-0000" ||
		empty.Scratch[0].Kind != "file" || !empty.Scratch[0].Present ||
		!empty.Scratch[0].ContentIsOpaque || empty.Scratch[0].Content != "" ||
		empty.Scratch[1] != (ProbeScratch{Name: "working-tree", Kind: "directory"}) {
		t.Fatalf("empty exact working input = %#v", empty.Scratch)
	}
	step := empty.Steps[0]
	separator := slices.Index(step.Arguments, "--")
	if separator < 0 || separator+1 >= len(step.Arguments) || step.Arguments[separator+1] != "${scratch:working-input-0000}" {
		t.Fatalf("working input transport argv = %q", step.Arguments)
	}
	encoded := slices.Index(step.Arguments, "-script_content_base64")
	if encoded < 0 || encoded >= separator || encoded+1 >= len(step.Arguments) {
		t.Fatalf("evaluated script must precede positional transports: %q", step.Arguments)
	}
	body, err := base64.StdEncoding.DecodeString(step.Arguments[encoded+1])
	if err != nil {
		t.Fatal(err)
	}
	if want := `cp -- "${1}" '.config'`; !strings.Contains(string(body), want) {
		t.Errorf("working-tree setup body %q omits %q", body, want)
	}
	validate := empty.Steps[2]
	validateEncoded := slices.Index(validate.Arguments, "-script_content_base64")
	validationBody, err := base64.StdEncoding.DecodeString(validate.Arguments[validateEncoded+1])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`cmp -s '.config' "${1}"`, `! -path './.config'`} {
		if !strings.Contains(string(validationBody), want) {
			t.Errorf("working-tree validation body %q omits %q", validationBody, want)
		}
	}

	nonempty, nonemptyNode := requestFor("CONFIG_CHANGED=y\n")
	if len(nonempty.Scratch) != 2 || nonempty.Scratch[0].Content != "CONFIG_CHANGED=y\n" ||
		!nonempty.Scratch[0].Present || !nonempty.Scratch[0].ContentIsOpaque {
		t.Fatalf("nonempty exact working input = %#v", nonempty.Scratch)
	}
	if emptyNode.ID == nonemptyNode.ID || emptyNode.RequestID == nonemptyNode.RequestID {
		t.Fatalf("working-tree content did not affect request identity: empty=%#v nonempty=%#v", emptyNode, nonemptyNode)
	}
}

func evaluatedScriptOutputResultSteps(request ProbeRequest, envelope string) []ProbeStepResult {
	steps := make([]ProbeStepResult, 0, len(request.Steps))
	for _, step := range request.Steps {
		result := ProbeStepResult{Name: step.Name, Status: "success", ExitCode: 0}
		if step.Name == "validate-evaluated-script-output" {
			result.Stdout = envelope
		}
		steps = append(steps, result)
	}
	return steps
}

func TestKbuildEvaluatedScriptOutputTextAcrossScopesRequiresConsensus(t *testing.T) {
	root, _ := sourceScriptOutputFixture(t)
	mustWriteSource(t, root, "kernel/time/timeconst.bc", "fixture\n")
	target, recipe := evaluatedScriptOutputRecipeForTest()
	discovery, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
	if err != nil {
		t.Fatal(err)
	}
	discoveryScopes := testSourceScriptOutputDualScopes(t, root, discovery, nil)
	if text, concrete, recognized, err := discoveryScopes.EvaluatedScriptOutputTextAcrossScopes(
		target, recipe, []string{"kernel/time/timeconst.bc"}, nil,
	); err != nil || text != "" || concrete || !recognized {
		t.Fatalf("cross-scope discovery = (%q,%t,%t,%v), want pending recognized result", text, concrete, recognized, err)
	}
	plan, err := discovery.Plan(discoveryScopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 2 || plan.Nodes[0].Scope == plan.Nodes[1].Scope {
		t.Fatalf("cross-scope discovery nodes = %#v, want target and host", plan.Nodes)
	}

	const exact = "#define GENERATED_VALUE 250\n"
	for _, test := range []struct {
		name       string
		host       string
		recognized bool
	}{
		{name: "identical", host: exact, recognized: true},
		{name: "scope sensitive", host: "#define GENERATED_VALUE 1000\n", recognized: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			results := probeResultMap{}
			for _, node := range plan.Nodes {
				request := plan.Requests[node.RequestID]
				contents := exact
				if node.Scope == "host" {
					contents = test.host
				}
				envelope := linuxProbeEvaluatedScriptSafePrefix + base64.StdEncoding.EncodeToString([]byte(contents)) + "\n"
				results[node.ID] = ProbeResult{
					Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
					Scope: node.Scope, ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: envelope,
					Steps: evaluatedScriptOutputResultSteps(request, envelope),
				}
			}
			replay, err := NewProbePlanBuilder(bootstrapTestIdentity, bootstrapTestIdentity)
			if err != nil {
				t.Fatal(err)
			}
			replayScopes := testSourceScriptOutputDualScopes(t, root, replay, results)
			got, concrete, recognized, err := replayScopes.EvaluatedScriptOutputTextAcrossScopes(
				target, recipe, []string{"kernel/time/timeconst.bc"}, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !concrete || recognized != test.recognized {
				t.Fatalf("cross-scope replay = (%q,%t,%t), want concrete recognized=%t", got, concrete, recognized, test.recognized)
			}
			if test.recognized && got != exact || !test.recognized && got != "" {
				t.Fatalf("cross-scope replay text = %q, recognized=%t", got, recognized)
			}
		})
	}
}

func TestKbuildEvaluatedScriptOutputTextReplayReturnsMeasuredBytes(t *testing.T) {
	root, _ := sourceScriptOutputFixture(t)
	mustWriteSource(t, root, "kernel/time/timeconst.bc", "fixture\n")
	target, recipe := evaluatedScriptOutputRecipeForTest()
	discovery, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	discoveryScopes := testSourceScriptOutputScopes(t, root, discovery, nil)
	if _, concrete, recognized, err := discoveryScopes.EvaluatedScriptOutputText(
		"target", target, recipe, []string{"kernel/time/timeconst.bc"}, nil,
	); err != nil || concrete || !recognized {
		t.Fatalf("discovery = concrete %t, recognized %t, %v", concrete, recognized, err)
	}
	plan, err := discovery.Plan(discoveryScopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	node := plan.Nodes[0]
	request := plan.Requests[node.RequestID]
	const exact = "#include <linux/param.h>\n#if HZ == 250\n#define HZ_TO_MSEC 4\n#endif\n"
	envelope := linuxProbeEvaluatedScriptSafePrefix + base64.StdEncoding.EncodeToString([]byte(exact)) + "\n"
	result := ProbeResult{
		Schema: LinuxProbeResultSchema, NodeID: node.ID, RequestID: node.RequestID,
		Scope: "target", ToolsetIdentity: bootstrapTestIdentity, Kind: "text", Text: envelope,
		Steps: evaluatedScriptOutputResultSteps(request, envelope),
	}
	replay, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
	if err != nil {
		t.Fatal(err)
	}
	replayScopes := testSourceScriptOutputScopes(t, root, replay, probeResultMap{node.ID: result})
	got, concrete, recognized, err := replayScopes.EvaluatedScriptOutputText(
		"target", target, recipe, []string{"kernel/time/timeconst.bc"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !concrete || !recognized || got != exact {
		t.Fatalf("replay result = (%q,%t,%t), want exact measured bytes", got, concrete, recognized)
	}
}

func TestKbuildEvaluatedScriptOutputTextFailsClosed(t *testing.T) {
	root, _ := sourceScriptOutputFixture(t)
	mustWriteSource(t, root, "kernel/time/timeconst.bc", "fixture\n")
	newScopes := func() (*KbuildProbeScopes, *ProbePlanBuilder) {
		builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
		if err != nil {
			t.Fatal(err)
		}
		return testSourceScriptOutputScopes(t, root, builder, nil), builder
	}
	target, recipe := evaluatedScriptOutputRecipeForTest()
	for name, candidate := range map[string]string{
		"dynamic shell parameter":   strings.Replace(recipe, "echo 250", "echo $CONFIG_HZ", 1),
		"shell state introspection": "{ set | grep probe_root | wc -l; } > " + target,
		"process introspection":     "{ ps; } > " + target,
		"file metadata":             "{ stat .config; } > " + target,
		"source awk program":        "{ awk -f ${tree:kernel}/kernel/time/timeconst.bc; } > " + target,
		"source sed program":        "{ sed -f ${tree:kernel}/kernel/time/timeconst.bc; } > " + target,
		"metadata predicate":        "{ test .config -nt ${tree:kernel}/kernel/time/timeconst.bc; } > " + target,
		"object tree input":         strings.Replace(recipe, "${tree:kernel}", "${tree:prep}", 1),
		"second output":             strings.Replace(recipe, "; }", "> leaked.txt; }", 1),
		"path command":              strings.Replace(recipe, "bc", "tools/bc", 1),
		"output command argument":   recipe + "; chmod +x " + target,
		"output ancestor argument":  recipe + "; chmod 777 include/generated",
		"working root argument":     recipe + "; chmod 777 .",
		"relative source operand":   strings.Replace(recipe, "${tree:kernel}/kernel/time/timeconst.bc", "kernel/time/timeconst.bc", 1),
		"implicit absolute operand": strings.Replace(recipe, "bc -q", "bc if=/proc/cpuinfo", 1),
		"nested command language":   strings.Replace(recipe, "bc -q ${tree:kernel}/kernel/time/timeconst.bc", "sh -c 'printf leak > /tmp/leak'", 1),
		"tree marker sibling":       strings.Replace(recipe, "${tree:kernel}/kernel/time/timeconst.bc", "${tree:kernel}.sibling", 1),
		"observed deletion":         strings.Replace(recipe, "echo 250", "rm -f sibling; echo 250", 1),
	} {
		t.Run(name, func(t *testing.T) {
			scopes, builder := newScopes()
			if _, concrete, recognized, err := scopes.EvaluatedScriptOutputText(
				"target", target, candidate, []string{"kernel/time/timeconst.bc"}, nil,
			); err != nil || concrete || recognized {
				t.Fatalf("unsupported recipe = concrete %t, recognized %t, error %v", concrete, recognized, err)
			}
			plan, err := builder.Plan(scopes.References()...)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Nodes) != 0 {
				t.Fatalf("unsupported recipe registered probes: %#v", plan.Nodes)
			}
		})
	}

	if err := os.Symlink("timeconst.bc", filepath.Join(root, "kernel/time/link.bc")); err != nil {
		t.Fatal(err)
	}
	symlinkRecipe := strings.Replace(recipe, "kernel/time/timeconst.bc", "kernel/time/link.bc", 1)
	scopes, builder := newScopes()
	if _, concrete, recognized, err := scopes.EvaluatedScriptOutputText(
		"target", target, symlinkRecipe, []string{"kernel/time/link.bc"}, nil,
	); err != nil || concrete || recognized {
		t.Fatalf("symlink prerequisite = concrete %t, recognized %t, error %v; want conservative fallback", concrete, recognized, err)
	}
	plan, err := builder.Plan(scopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 0 {
		t.Fatalf("symlink prerequisite registered probes: %#v", plan.Nodes)
	}

	scopes, builder = newScopes()
	scopes.evaluators["target"].scriptEnvironment["BC_ENV_ARGS"] = "/undeclared/program.bc"
	if _, concrete, recognized, err := scopes.EvaluatedScriptOutputText(
		"target", target, recipe, []string{"kernel/time/timeconst.bc"}, nil,
	); err != nil || concrete || recognized {
		t.Fatalf("BC_ENV_ARGS recipe = concrete %t, recognized %t, error %v; want conservative fallback", concrete, recognized, err)
	}
	plan, err = builder.Plan(scopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 0 {
		t.Fatalf("BC_ENV_ARGS recipe registered probes: %#v", plan.Nodes)
	}

	tooMany := make([]string, maxProbeSources)
	for index := range tooMany {
		tooMany[index] = "kernel/time/timeconst.bc"
	}
	scopes, builder = newScopes()
	if _, concrete, recognized, err := scopes.EvaluatedScriptOutputText(
		"target", target, recipe, tooMany, nil,
	); err != nil || concrete || recognized {
		t.Fatalf("oversized source frontier = concrete %t, recognized %t, error %v; want conservative fallback", concrete, recognized, err)
	}
	plan, err = builder.Plan(scopes.References()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Nodes) != 0 {
		t.Fatalf("oversized source frontier registered probes: %#v", plan.Nodes)
	}
}

func TestKbuildSourceScriptOutputTextRejectsUntypedProtocolAuthority(t *testing.T) {
	root, valid := sourceScriptOutputFixture(t)
	mustWriteSource(t, root, "data/not-a-script", "ordinary data\n")
	newScopes := func(t *testing.T) *KbuildProbeScopes {
		builder, err := NewProbePlanBuilder(bootstrapTestIdentity, "")
		if err != nil {
			t.Fatal(err)
		}
		return testSourceScriptOutputScopes(t, root, builder, nil)
	}
	tests := []struct {
		name      string
		script    string
		arguments []KbuildSourceScriptArgument
		want      string
	}{
		{name: "no output", script: "scripts/render.sh", arguments: valid[1:], want: "exactly one output"},
		{name: "two outputs", script: "scripts/render.sh", arguments: append(slices.Clone(valid), KbuildSourceScriptArgument{Kind: KbuildSourceScriptOutputArgument}), want: "exactly one output"},
		{name: "named output", script: "scripts/render.sh", arguments: []KbuildSourceScriptArgument{{Kind: KbuildSourceScriptOutputArgument, Value: "ambient"}}, want: "nonempty value"},
		{name: "named stdout", script: "scripts/render.sh", arguments: []KbuildSourceScriptArgument{{Kind: KbuildSourceScriptStdoutArgument, Value: "ambient"}}, want: "nonempty value"},
		{name: "two stdout outputs", script: "scripts/render.sh", arguments: []KbuildSourceScriptArgument{{Kind: KbuildSourceScriptStdoutArgument}, {Kind: KbuildSourceScriptStdoutArgument}}, want: "exactly one output"},
		{name: "mixed outputs", script: "scripts/render.sh", arguments: append(slices.Clone(valid), KbuildSourceScriptArgument{Kind: KbuildSourceScriptStdoutArgument}), want: "exactly one output"},
		{name: "escaping source", script: "scripts/render.sh", arguments: []KbuildSourceScriptArgument{{Kind: KbuildSourceScriptOutputArgument}, {Kind: KbuildSourceScriptSourceArgument, Value: "../outside"}}, want: "canonical source-relative"},
		{name: "reserved literal", script: "scripts/render.sh", arguments: []KbuildSourceScriptArgument{{Kind: KbuildSourceScriptOutputArgument}, {Kind: KbuildSourceScriptLiteralArgument, Value: "${tool:cc}"}}, want: "reserved probe placeholder"},
		{name: "unavailable tool", script: "scripts/render.sh", arguments: []KbuildSourceScriptArgument{{Kind: KbuildSourceScriptOutputArgument}, {Kind: KbuildSourceScriptToolArgument, Value: "ambient"}}, want: "unavailable role"},
		{name: "unknown kind", script: "scripts/render.sh", arguments: []KbuildSourceScriptArgument{{Kind: KbuildSourceScriptOutputArgument}, {Kind: "ambient", Value: "value"}}, want: "unsupported kind"},
		{name: "absolute script", script: filepath.Join(root, "scripts/render.sh"), arguments: valid, want: "canonical source-relative"},
		{name: "nonscript source", script: "data/not-a-script", arguments: valid, want: "not a shell script"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := newScopes(t).SourceScriptOutputText("target", test.script, test.arguments)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}
