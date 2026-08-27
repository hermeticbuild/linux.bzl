package kconfig

// Linux occasionally expresses one compiler capability as a compound
// scripts/Makefile.compiler try-run instead of one compiler invocation. Keep
// that source-owned shell program intact: it executes through the registered
// script runtime, with configured tools exposed only as private proxies and
// with TMP bound to the probe action's private scratch directory.

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

type compoundTryRunReplacement struct {
	start int
	end   int
	value string
}

// compoundTryRunProbe recognizes a bounded shell command graph containing at
// least one selected configured tool. The graph and every compiler argument
// remain Linux source data; this layer validates only shell authority and
// rewrites configured executable spellings to scriptrun's private proxies.
// Capability text embedded from earlier probes is supplied as the exact script
// stdin, so Make expansion still happens before shell parsing and field
// splitting exactly as it does upstream.
func (e *LinuxProbeEvaluator) compoundTryRunProbe(command string) (linuxProbeTruth, bool, error) {
	looksCompound := strings.Contains(command, "&&") || strings.Contains(command, "||") ||
		strings.Contains(command, ";") || strings.Contains(command, ">")
	if !looksCompound {
		return linuxProbeTruth{}, false, nil
	}
	if strings.Contains(command, compactKbuildLiteralDollarToken) {
		return linuxProbeTruth{}, e.ownsProbeCommand(command), fmt.Errorf("compound try-run contains a reserved lexer token")
	}
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		if e.ownsProbeCommand(command) {
			return linuxProbeTruth{}, true, fmt.Errorf("lex compound try-run: %w", err)
		}
		return linuxProbeTruth{}, false, nil
	}

	expectProgram := true
	expectRedirect := false
	compoundOperator := false
	configuredRoles := map[string]bool{}
	runtimePrograms := map[string]bool{"sh": true}
	replacements := []compoundTryRunReplacement{}
	for index, token := range tokens {
		if token.operator {
			switch token.value {
			case "|", "&&", "||", ";":
				if expectProgram || expectRedirect {
					return linuxProbeTruth{}, true, fmt.Errorf("compound try-run has misplaced operator %q", token.value)
				}
				if token.value != "|" {
					compoundOperator = true
				}
				expectProgram = true
			case ">", ">>":
				if expectProgram || expectRedirect {
					return linuxProbeTruth{}, true, fmt.Errorf("compound try-run has misplaced redirection %q", token.value)
				}
				compoundOperator = true
				expectRedirect = true
			default:
				return linuxProbeTruth{}, true, fmt.Errorf("compound try-run uses unsupported operator %q", token.value)
			}
			continue
		}

		if expectRedirect {
			if !compoundTryRunScratchWord(token.value) {
				return linuxProbeTruth{}, true, fmt.Errorf("compound try-run redirects outside private TMP: %q", token.value)
			}
			expectRedirect = false
			continue
		}
		if expectProgram {
			role, selected, roleErr := e.compoundTryRunConfiguredToolRole(token.value)
			if roleErr != nil {
				return linuxProbeTruth{}, true, fmt.Errorf("compound try-run command %d: %w", index, roleErr)
			}
			if selected {
				if role == linuxProbeScriptRunner || role == linuxProbeScriptRuntime || strings.HasPrefix(role, compactKbuildScriptAppletRolePrefix) {
					return linuxProbeTruth{}, true, fmt.Errorf("compound try-run invokes infrastructure role %q", role)
				}
				configuredRoles[role] = true
				replacements = append(replacements, compoundTryRunReplacement{start: token.start, end: token.end, value: role})
			} else {
				program := strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$")
				if !compoundTryRunRuntimeProgram(program) {
					if e.ownsProbeCommand(command) {
						return linuxProbeTruth{}, true, fmt.Errorf("compound try-run has non-hermetic program %q", program)
					}
					return linuxProbeTruth{}, false, nil
				}
				runtimePrograms[program] = true
			}
			expectProgram = false
			continue
		}

		if role, selected, roleErr := e.compoundTryRunConfiguredToolRole(token.value); roleErr != nil {
			return linuxProbeTruth{}, true, fmt.Errorf("compound try-run argument %d: %w", index, roleErr)
		} else if selected {
			return linuxProbeTruth{}, true, fmt.Errorf("compound try-run passes configured tool role %q as data", role)
		}
		if err := validateCompoundTryRunWord(token.value); err != nil {
			return linuxProbeTruth{}, true, fmt.Errorf("compound try-run argument %d: %w", index, err)
		}
		if linuxProbeSymbolPattern.MatchString(token.value) {
			if err := e.validateCompoundTryRunSymbolicWord(token.value); err != nil {
				return linuxProbeTruth{}, true, fmt.Errorf("compound try-run argument %d: %w", index, err)
			}
		}
	}
	if !compoundOperator {
		return linuxProbeTruth{}, false, nil
	}
	if len(tokens) == 0 || expectProgram || expectRedirect || len(configuredRoles) == 0 {
		if e.ownsProbeCommand(command) {
			return linuxProbeTruth{}, true, e.unsupportedCommand(command)
		}
		return linuxProbeTruth{}, false, nil
	}
	if e.tools[linuxProbeScriptRunner] == "" || e.tools[linuxProbeScriptRuntime] == "" {
		return linuxProbeTruth{}, true, fmt.Errorf("compound try-run requires configured %s and %s roles", linuxProbeScriptRunner, linuxProbeScriptRuntime)
	}

	script := applyCompoundTryRunReplacements(command, replacements)
	script = probeCompilerRootArgument(e.canonicalSourceCommand(script))
	// The upstream wrapper assigns TMP as a non-exported shell variable. Pass
	// the private scratch path as argv instead of process environment so the
	// compiler and nm observe the same environment as the source try-run.
	script = `TMP="$1"; shift; ` + script
	lowerer := newProbeSymbolicValueLowerer(e)
	stdinFragments, symbolic, err := lowerer.value(script)
	if err != nil {
		return linuxProbeTruth{}, true, fmt.Errorf("lower compound try-run script: %w", err)
	}
	stdinFragments = compoundTryRunRootFragments(e.canonicalSourceFragments(stdinFragments))
	stdin := script
	if symbolic {
		stdin = ""
	}
	usesHostDeps := strings.Contains(stdin, "${source_root:"+linuxProbeHostDepsRootName+"}") ||
		compoundTryRunFragmentsContain(stdinFragments, "${source_root:"+linuxProbeHostDepsRootName+"}")

	roles := make([]string, 0, len(configuredRoles))
	for role := range configuredRoles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	arguments := []string{
		"-interpreter", "${tool:" + linuxProbeScriptRuntime + "}",
		"-interpreter_arg", "sh",
		"-multicall", "${tool:" + linuxProbeScriptRuntime + "}",
		"-script_stdin",
	}
	for _, role := range roles {
		arguments = append(arguments, "-tool", role+"=${tool:"+role+"}")
	}

	registered := make([]KbuildActionRoleRef, 0, len(e.tools))
	for role := range e.tools {
		registered = append(registered, KbuildActionRoleRef{Scope: e.scope, Role: role})
	}
	applets, err := compactKbuildScriptRuntimeApplets(registered, e.scope)
	if err != nil {
		return linuxProbeTruth{}, true, err
	}
	auxiliary := slices.Clone(roles)
	for _, applet := range applets {
		arguments = append(arguments, "-applet", applet.name+"=${tool:"+applet.role+"}")
		auxiliary = append(auxiliary, applet.role)
	}
	sort.Strings(auxiliary)
	auxiliary = slices.Compact(auxiliary)
	programs := make([]string, 0, len(runtimePrograms))
	for program := range runtimePrograms {
		programs = append(programs, program)
	}
	sort.Strings(programs)
	for _, program := range programs {
		arguments = append(arguments, "-require_applet", program)
	}
	arguments = append(arguments, "--", "${scratch:tmp}")

	sourceRoots := []string{linuxProbeSourceRootName}
	if usesHostDeps {
		sourceRoots = append(sourceRoots, linuxProbeHostDepsRootName)
	}
	sort.Strings(sourceRoots)
	sourceRoots = slices.Compact(sourceRoots)
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema, InputCount: len(lowerer.dependencies),
		Sources: []string{linuxProbeRootAnchor}, SourceRoots: sourceRoots,
		Scratch: []ProbeScratch{{Name: "tmp", Kind: "file"}},
		Steps: []ProbeStep{{
			Name: "compound-try-run", Tool: linuxProbeScriptRunner,
			AuxiliaryTools: auxiliary, WorkingDirectory: "${source_root:" + linuxProbeSourceRootName + "}",
			Arguments: arguments, Stdin: stdin, StdinFragments: stdinFragments,
			DiscardStdout: true, DiscardStderr: true,
		}},
		Outcome: ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "exit-zero", Step: "compound-try-run"}},
	}
	if err := request.Validate(); err != nil {
		return linuxProbeTruth{}, true, fmt.Errorf("compound try-run request: %w", err)
	}
	truth, err := e.requestTruth(request, lowerer.dependencies...)
	return truth, true, err
}

func validateCompoundTryRunWord(value string) error {
	restored := strings.ReplaceAll(value, compactKbuildLiteralDollarToken, "$")
	if strings.Contains(restored, "${") {
		return fmt.Errorf("word collides with probe placeholder syntax: %q", value)
	}
	withoutSymbols := linuxProbeSymbolPattern.ReplaceAllString(value, "")
	withoutSymbols = strings.ReplaceAll(withoutSymbols, compactKbuildLiteralDollarToken, "")
	if strings.ContainsRune(withoutSymbols, '`') || strings.Contains(withoutSymbols, "$(") || strings.Contains(withoutSymbols, "${") {
		return fmt.Errorf("word contains active shell substitution: %q", value)
	}
	if strings.ContainsRune(withoutSymbols, '$') && !compoundTryRunScratchWord(withoutSymbols) {
		return fmt.Errorf("word contains unsupported shell parameter expansion: %q", value)
	}
	if strings.HasPrefix(value, "@") {
		return fmt.Errorf("word selects a response file: %q", value)
	}
	return nil
}

func (e *LinuxProbeEvaluator) compoundTryRunConfiguredToolRole(value string) (string, bool, error) {
	selected := ""
	for role, token := range e.tools {
		if token == "" {
			continue
		}
		if _, companion := toolaction.BaseContractRole(role); companion {
			continue
		}
		if !e.isToolToken(value, role) {
			continue
		}
		if selected != "" && selected != role {
			return "", true, fmt.Errorf("configured tool token %q is ambiguous between roles %s and %s", value, selected, role)
		}
		selected = role
	}
	return selected, selected != "", nil
}

// Compound try-runs may use the registered shell only as control flow around
// selected tools and these bounded scalar reducers. Source scripts remain the
// separate, explicitly declared escape hatch for broader source-owned logic.
func compoundTryRunRuntimeProgram(program string) bool {
	if !safeLinuxSourceScriptCommandName(program) {
		return false
	}
	switch program {
	case "cmp", "echo", "false", "grep", "printf", "true":
		return true
	default:
		return false
	}
}

func (e *LinuxProbeEvaluator) validateCompoundTryRunSymbolicWord(value string) error {
	scope := e.scope
	if scope != "target" && scope != "host" {
		scope = "target"
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{scope: e}}
	bindings, symbols, hasText, err := scopes.collectSymbolicComparison(value)
	if err != nil {
		return err
	}
	if hasText || len(bindings) > maxKbuildSymbolicComparisonReferences {
		return fmt.Errorf("symbolic shell word is not a bounded finite selection")
	}
	for state := 0; state < 1<<len(bindings); state++ {
		expanded, err := expandSymbolicComparison(value, state, bindings, symbols)
		if err != nil {
			return err
		}
		tokens, err := lexCompactKbuildRecipe(expanded)
		if err != nil {
			return fmt.Errorf("symbolic branch %q cannot be lexed: %w", expanded, err)
		}
		for _, token := range tokens {
			if token.operator {
				return fmt.Errorf("symbolic branch %q introduces shell operator %q", expanded, token.value)
			}
			if err := validateCompoundTryRunWord(token.value); err != nil {
				return fmt.Errorf("symbolic branch %q: %w", expanded, err)
			}
		}
	}
	return nil
}

func compoundTryRunRootFragments(fragments []ProbeValueFragment) []ProbeValueFragment {
	fragments = slices.Clone(fragments)
	for index := range fragments {
		fragment := fragments[index]
		fragment.Value = probeCompilerRootArgument(fragment.Value)
		fragment.Fragments = compoundTryRunRootFragments(fragment.Fragments)
		fragment.Transforms = slices.Clone(fragment.Transforms)
		for transformIndex := range fragment.Transforms {
			transform := fragment.Transforms[transformIndex]
			transform.Arguments = slices.Clone(transform.Arguments)
			for argumentIndex := range transform.Arguments {
				transform.Arguments[argumentIndex] = probeCompilerRootArgument(transform.Arguments[argumentIndex])
			}
			transform.ArgumentFragments = slices.Clone(transform.ArgumentFragments)
			for groupIndex := range transform.ArgumentFragments {
				group := transform.ArgumentFragments[groupIndex]
				group.Fragments = compoundTryRunRootFragments(group.Fragments)
				transform.ArgumentFragments[groupIndex] = group
			}
			fragment.Transforms[transformIndex] = transform
		}
		fragments[index] = fragment
	}
	return fragments
}

func compoundTryRunFragmentsContain(fragments []ProbeValueFragment, value string) bool {
	for _, fragment := range fragments {
		if strings.Contains(fragment.Value, value) || compoundTryRunFragmentsContain(fragment.Fragments, value) {
			return true
		}
		for _, transform := range fragment.Transforms {
			for _, argument := range transform.Arguments {
				if strings.Contains(argument, value) {
					return true
				}
			}
			for _, group := range transform.ArgumentFragments {
				if compoundTryRunFragmentsContain(group.Fragments, value) {
					return true
				}
			}
		}
	}
	return false
}

func compoundTryRunScratchWord(value string) bool {
	value = strings.ReplaceAll(value, compactKbuildLiteralDollarToken, "")
	if value == "$TMP" {
		return true
	}
	if !strings.HasPrefix(value, "$TMP.") || len(value) == len("$TMP.") {
		return false
	}
	for _, character := range value[len("$TMP."):] {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("_+.-", character) {
			continue
		}
		return false
	}
	return true
}

func applyCompoundTryRunReplacements(command string, replacements []compoundTryRunReplacement) string {
	slices.SortFunc(replacements, func(left, right compoundTryRunReplacement) int { return left.start - right.start })
	var out strings.Builder
	start := 0
	for _, replacement := range replacements {
		out.WriteString(command[start:replacement.start])
		out.WriteString(replacement.value)
		start = replacement.end
	}
	out.WriteString(command[start:])
	return out.String()
}
