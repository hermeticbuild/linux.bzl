package kconfig

// This file lowers Linux's HOSTPKG_CONFIG flag queries into the Kbuild probe
// DAG. The query producer is a configured host action role backed by a
// declared package manifest; neither the planner's PATH nor a worker-local
// pkg-config database participates. The measured text is replayed before
// target recipes are selected, so a package may contribute any number of
// compiler or linker words without late generated-text field splitting.

import (
	"fmt"
	"strings"
)

const linuxProbePkgConfigRole = "pkg-config"

func (e *LinuxProbeEvaluator) configuredPkgConfigQuery(command string) (string, bool, error) {
	request, recognized, err := e.configuredPkgConfigQueryRequest(command)
	if err != nil || !recognized {
		return "", recognized, err
	}
	value, err := e.requestText(request)
	return value, true, err
}

// configuredPkgConfigQueryRequest accepts the two forms used by upstream
// Linux host-tool Makefiles:
//
//	CONFIGURED_PKG_CONFIG --cflags PACKAGE 2>/dev/null
//	CONFIGURED_PKG_CONFIG PACKAGE --libs 2>/dev/null || echo LITERAL
//
// The configured shim validates the package/output-mode grammar again. This
// parser bounds shell authority: every query word and fallback is literal,
// stderr may only be discarded to /dev/null, and no other control flow is
// admitted. scriptrun supplies the configured role through a private proxy.
func (e *LinuxProbeEvaluator) configuredPkgConfigQueryRequest(command string) (ProbeRequest, bool, error) {
	if e == nil {
		return ProbeRequest{}, false, fmt.Errorf("Linux pkg-config probe evaluator is nil")
	}
	command = strings.TrimSpace(command)
	if command == "" || len(command) > 1<<16 || strings.ContainsAny(command, "\x00\r\n") {
		return ProbeRequest{}, false, nil
	}
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		if strings.Contains(command, kbuildActionRoleTokenPrefix) {
			return ProbeRequest{}, true, fmt.Errorf("lex configured pkg-config query: %w", err)
		}
		return ProbeRequest{}, false, nil
	}
	if len(tokens) == 0 || tokens[0].operator {
		return ProbeRequest{}, false, nil
	}
	ref, selected := parseKbuildActionRoleToken(tokens[0].value)
	if !selected || ref.Role != linuxProbePkgConfigRole {
		return ProbeRequest{}, false, nil
	}
	if ref.Scope != "host" || e.scope != "host" {
		return ProbeRequest{}, true, fmt.Errorf("configured pkg-config query requires the host action scope")
	}
	if e.tools[linuxProbePkgConfigRole] == "" {
		return ProbeRequest{}, true, fmt.Errorf("configured pkg-config query references an unavailable host action role")
	}
	if e.tools[linuxProbeScriptRunner] == "" || e.tools[linuxProbeScriptRuntime] == "" {
		return ProbeRequest{}, true, fmt.Errorf(
			"configured pkg-config query requires configured %s and %s roles",
			linuxProbeScriptRunner, linuxProbeScriptRuntime,
		)
	}

	primaryEnd := len(tokens)
	fallbackIndex := -1
	for index := 1; index < len(tokens); index++ {
		if !tokens[index].operator {
			continue
		}
		switch tokens[index].value {
		case ">":
			// The exact descriptor-qualified discard is validated below.
		case "||":
			if fallbackIndex >= 0 {
				return ProbeRequest{}, true, fmt.Errorf("configured pkg-config query repeats its fallback operator")
			}
			fallbackIndex = index
			primaryEnd = index
		default:
			return ProbeRequest{}, true, fmt.Errorf("configured pkg-config query uses unsupported operator %q", tokens[index].value)
		}
	}
	if primaryEnd < 5 {
		return ProbeRequest{}, true, fmt.Errorf("configured pkg-config query omits its exact stderr discard")
	}
	redirect := tokens[primaryEnd-3 : primaryEnd]
	if redirect[0].operator || redirect[0].value != "2" ||
		!redirect[1].operator || redirect[1].value != ">" ||
		redirect[2].operator || redirect[2].value != "/dev/null" ||
		redirect[0].end != redirect[1].start ||
		command[redirect[0].start:redirect[0].end] != "2" ||
		command[redirect[2].start:redirect[2].end] != "/dev/null" {
		return ProbeRequest{}, true, fmt.Errorf("configured pkg-config query requires exact stderr discard 2>/dev/null")
	}
	for _, token := range tokens[1 : primaryEnd-3] {
		if token.operator {
			return ProbeRequest{}, true, fmt.Errorf("configured pkg-config query has an operator in its argument list")
		}
	}
	queryArguments, err := configuredPkgConfigLiteralArguments(command, tokens[1:primaryEnd-3])
	if err != nil {
		return ProbeRequest{}, true, err
	}
	if err := validateConfiguredPkgConfigArguments(queryArguments); err != nil {
		return ProbeRequest{}, true, err
	}

	fallback := ""
	if fallbackIndex >= 0 {
		fallbackTokens := tokens[fallbackIndex+1:]
		if len(fallbackTokens) != 2 || fallbackTokens[0].operator || fallbackTokens[0].value != "echo" || fallbackTokens[1].operator ||
			command[fallbackTokens[0].start:fallbackTokens[0].end] != "echo" {
			return ProbeRequest{}, true, fmt.Errorf("configured pkg-config query fallback must be exactly echo LITERAL")
		}
		fallback, err = configuredPkgConfigLiteralWord(command, fallbackTokens[1])
		if err != nil {
			return ProbeRequest{}, true, fmt.Errorf("configured pkg-config query fallback: %w", err)
		}
		if !validConfiguredPkgConfigFallback(fallback) {
			return ProbeRequest{}, true, fmt.Errorf("configured pkg-config query has unsafe fallback literal %q", fallback)
		}
	}

	scriptWords := []string{linuxProbePkgConfigRole}
	for _, argument := range queryArguments {
		scriptWords = append(scriptWords, sourceShellQueryScriptWord(argument))
	}
	script := strings.Join(scriptWords, " ") + " 2>/dev/null"
	if fallback != "" {
		script += " || echo " + sourceShellQueryScriptWord(fallback)
	} else {
		// GNU Make's shell function consumes stdout even when the command exits
		// nonzero. Keep the script action successful while scriptrun still fails
		// on an invalid interpreter, runtime, proxy, or configured executable.
		script += " || :"
	}
	arguments := []string{
		"-interpreter", "${tool:" + linuxProbeScriptRuntime + "}",
		"-interpreter_arg", "sh",
		"-multicall", "${tool:" + linuxProbeScriptRuntime + "}",
		"-script_content", script,
		"-tool", linuxProbePkgConfigRole + "=${tool:" + linuxProbePkgConfigRole + "}",
		"-require_applet", "sh",
		"--",
	}
	const stepName = "configured-pkg-config-query"
	request := ProbeRequest{
		Schema: LinuxProbeRequestSchema,
		Steps: []ProbeStep{{
			Name: stepName, Tool: linuxProbeScriptRunner,
			AuxiliaryTools: []string{linuxProbePkgConfigRole},
			Arguments:      arguments,
		}},
		Outcome: ProbeOutcome{
			Kind: "text", Step: stepName, Stream: "stdout",
			GNUMakeShell: true, RequireSuccess: true,
		},
	}
	if err := request.Validate(); err != nil {
		return ProbeRequest{}, true, fmt.Errorf("configured pkg-config query request: %w", err)
	}
	return request, true, nil
}

func configuredPkgConfigLiteralArguments(command string, tokens []compactKbuildRecipeToken) ([]string, error) {
	arguments := make([]string, len(tokens))
	for index, token := range tokens {
		value, err := configuredPkgConfigLiteralWord(command, token)
		if err != nil {
			return nil, fmt.Errorf("configured pkg-config query argument %d: %w", index, err)
		}
		arguments[index] = value
	}
	return arguments, nil
}

func configuredPkgConfigLiteralWord(command string, token compactKbuildRecipeToken) (string, error) {
	if token.operator || token.start < 0 || token.end <= token.start || token.end > len(command) {
		return "", fmt.Errorf("has invalid literal provenance")
	}
	raw := command[token.start:token.end]
	value := strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$")
	if strings.Contains(token.value, compactKbuildLiteralDollarToken) || strings.Contains(value, "${") {
		return "", fmt.Errorf("contains dynamic or reserved shell text")
	}
	if raw != value && !sourceShellQueryQuotedLiteral(raw) {
		return "", fmt.Errorf("must be one literal shell word, got %q", raw)
	}
	return value, nil
}

func validateConfiguredPkgConfigArguments(arguments []string) error {
	mode := ""
	packages := 0
	if len(arguments) == 0 || len(arguments) > 64 {
		return fmt.Errorf("configured pkg-config query requires a bounded argument list")
	}
	for _, argument := range arguments {
		switch argument {
		case "--cflags", "--libs", "--exists":
			if mode != "" {
				return fmt.Errorf("configured pkg-config query repeats or combines output modes %q and %q", mode, argument)
			}
			mode = argument
		default:
			if !validConfiguredPkgConfigPackage(argument) {
				return fmt.Errorf("configured pkg-config query has unsupported package argument %q", argument)
			}
			packages++
		}
	}
	if mode == "" || packages == 0 {
		return fmt.Errorf("configured pkg-config query requires one output mode and at least one package")
	}
	return nil
}

func validConfiguredPkgConfigPackage(value string) bool {
	if value == "" || len(value) > 255 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("_.+-", character) {
			continue
		}
		return false
	}
	return true
}

func validConfiguredPkgConfigFallback(value string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("_@%+=:,./-", character) {
			continue
		}
		return false
	}
	return true
}
