package kconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// CompactKbuildCommandTemplate is one source-selected action expansion in rule
// recipe order. Name identifies a cmd_<name> leaf. An empty Name identifies a
// direct source occurrence: Source retains its selected Make spelling while
// Text is the evaluated executable text consumed by graph discovery and final
// generic lowering. It is exposed to recursive-invocation discovery so a Make
// call or immutable source script hidden behind if_changed remains part of the
// same source-owned control graph as a literal recipe line.
type CompactKbuildCommandTemplate struct {
	Name   string
	Source string
	Text   string
	// leaves retains the already-evaluated cmd_<name> values hidden behind a
	// composite wrapper marker. It is planner-local provenance used to replay
	// surrounding Make text functions exactly; it is never serialized.
	leaves map[string]string
}

// CompactKbuildRecipeIsExactCommandTemplateCall reports whether one recipe
// line consists only of a supported Kbuild command-template call. Callers
// which replace the outer if_changed/cmd wrapper with cmd_<name> must use this
// guard before assigning byte provenance: kbuildRecipeCommandExpressions also
// finds a call embedded in a larger shell line, whose remaining commands may
// rewrite the target.
func CompactKbuildRecipeIsExactCommandTemplateCall(recipe string) bool {
	recipe = strings.TrimSpace(recipe)
	for recipe != "" && strings.ContainsRune("@+-", rune(recipe[0])) {
		recipe = strings.TrimSpace(recipe[1:])
	}
	if !strings.HasPrefix(recipe, "$(call") {
		return false
	}
	end, err := matchingKbuildReference(recipe, 1)
	if err != nil || strings.TrimSpace(recipe[end+1:]) != "" {
		return false
	}
	function, arguments, ok := splitMakeFunction(recipe[2:end])
	if !ok || function != "call" || len(arguments) < 2 || strings.TrimSpace(arguments[1]) == "" {
		return false
	}
	wrapper := strings.TrimSpace(arguments[0])
	return wrapper == "cmd" || wrapper == "if_changed" || strings.HasPrefix(wrapper, "if_changed_")
}

// CompactKbuildSourceScript is an immutable shell program selected by one
// evaluated command template. Path is relative to the Linux source tree and
// Content is read from that exact tree while the invocation profile is alive.
type CompactKbuildSourceScript struct {
	Path    string
	Content string
}

// EvaluateCompactKbuildCommandTemplates expands only command-call expressions
// selected by the concrete rule. It does not interpret command names or shell
// behavior; consumers inspect the returned source text for their own generic
// protocol (for example, recursive Make argv discovery).
func EvaluateCompactKbuildCommandTemplates(
	profile CompactKbuildProfile,
	rule KbuildRule,
	target, stem string,
	injected map[string]string,
) ([]CompactKbuildCommandTemplate, error) {
	return evaluateCompactKbuildCommandTemplates(profile, rule, target, target, stem, injected, true)
}

// EvaluateCompactKbuildCommandTemplatesSymbolic retains compiler-probe atoms
// while discovering recursive Make hidden behind a selected command macro.
func EvaluateCompactKbuildCommandTemplatesSymbolic(
	profile CompactKbuildProfile,
	rule KbuildRule,
	target, stem string,
	injected map[string]string,
) ([]CompactKbuildCommandTemplate, error) {
	return evaluateCompactKbuildCommandTemplates(profile, rule, target, target, stem, injected, false)
}

// EvaluateCompactKbuildCommandTemplatesSymbolicForAutomaticTarget is the
// declaration-local counterpart of EvaluateCompactKbuildCommandTemplatesSymbolic.
// target remains the canonical graph identity while automaticTarget preserves
// the selected rule's Make spelling for $@.
func EvaluateCompactKbuildCommandTemplatesSymbolicForAutomaticTarget(
	profile CompactKbuildProfile,
	rule KbuildRule,
	target, automaticTarget, stem string,
	injected map[string]string,
) ([]CompactKbuildCommandTemplate, error) {
	return evaluateCompactKbuildCommandTemplates(
		profile, rule, target, automaticTarget, stem, injected, false,
	)
}

// EvaluateCompactKbuildCommandTemplatesSymbolicForMakeTarget also preserves
// the lexical implicit-rule/target-variable lookup identity. The rule's
// prerequisite slices must contain the exact expanded Make words.
func EvaluateCompactKbuildCommandTemplatesSymbolicForMakeTarget(
	profile CompactKbuildProfile,
	rule KbuildRule,
	target, lookupTarget, automaticTarget, stem string,
	injected map[string]string,
) ([]CompactKbuildCommandTemplate, error) {
	return evaluateCompactKbuildCommandTemplatesForMakeTarget(
		profile, rule, target, lookupTarget, automaticTarget, stem, injected, false,
	)
}

// ResolveCompactKbuildSymbolicText resolves compiler-probe atoms in text which
// has already been expanded in one captured profile. It deliberately performs
// no further Make expansion or word splitting: graph selection uses it only to
// observe the exact replay value of a command template after that template has
// registered all of its symbolic atoms. During discovery the installed
// resolver preserves those atoms, while replay resolves the same expression
// DAG from the result oracle.
func ResolveCompactKbuildSymbolicText(profile CompactKbuildProfile, value string) (string, error) {
	return ResolveCompactKbuildTargetSymbolicText(profile, "", value)
}

// ResolveCompactKbuildTargetSymbolicText resolves already-expanded symbolic
// text using the evaluator and exact probe process environment captured for
// target. This is the target-aware counterpart of
// ResolveCompactKbuildSymbolicText for action-plan consumers.
func ResolveCompactKbuildTargetSymbolicText(
	profile CompactKbuildProfile,
	target string,
	value string,
) (string, error) {
	resolved, err := ResolveCompactKbuildTargetSymbolicValue(profile, target, value)
	if err != nil {
		return "", err
	}
	return compactKbuildProfileCanonicalRecipeText(profile, resolved), nil
}

// ResolveCompactKbuildTargetSymbolicStructure selects the replay-time finite
// control branches in already-expanded recipe text while retaining nested
// probe-derived argv values. This gives recursive-Make discovery the concrete
// shell segment shape without changing the symbolic dependency graph carried
// into the child invocation.
func ResolveCompactKbuildTargetSymbolicStructure(
	profile CompactKbuildProfile,
	target string,
	value string,
) (string, error) {
	evaluator, err := compactKbuildProfileTargetEvaluator(profile, target)
	if err != nil {
		return "", err
	}
	resolved, err := evaluator.template.resolveKbuildSymbolicStructure(value)
	if err != nil {
		return "", fmt.Errorf("Kbuild profile %q target %q symbolic structure: %w", profile.Name, target, err)
	}
	return compactKbuildProfileCanonicalRecipeText(profile, resolved), nil
}

// ResolveCompactKbuildTargetSymbolicValue resolves probe atoms in an
// already-expanded GNU Make value without converting evaluator root sentinels
// into action-recipe placeholders. Recursive Make environment replay uses this
// form because the resolved value is parsed by Make again rather than executed
// as final recipe text.
func ResolveCompactKbuildTargetSymbolicValue(
	profile CompactKbuildProfile,
	target string,
	value string,
) (string, error) {
	evaluator, err := compactKbuildProfileTargetEvaluator(profile, target)
	if err != nil {
		return "", err
	}
	resolved, err := evaluator.template.resolveKbuildSymbolic(value)
	if err != nil {
		return "", fmt.Errorf("Kbuild profile %q target %q symbolic text: %w", profile.Name, target, err)
	}
	return compactKbuildProfileCanonicalMakeValue(profile, resolved), nil
}

func evaluateCompactKbuildCommandTemplates(
	profile CompactKbuildProfile,
	rule KbuildRule,
	target, automaticTarget, stem string,
	injected map[string]string,
	resolveSymbolic bool,
) ([]CompactKbuildCommandTemplate, error) {
	return evaluateCompactKbuildCommandTemplatesForMakeTarget(
		profile, rule, target, target, automaticTarget, stem, injected, resolveSymbolic,
	)
}

func evaluateCompactKbuildCommandTemplatesForMakeTarget(
	profile CompactKbuildProfile,
	rule KbuildRule,
	target, lookupTarget, automaticTarget, stem string,
	injected map[string]string,
	resolveSymbolic bool,
) ([]CompactKbuildCommandTemplate, error) {
	selections, err := evaluatedKbuildRuleCommandSelectionsForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, rule.Prerequisites, rule.OrderOnly,
		injected, rule.Recipe, resolveSymbolic,
	)
	if err != nil {
		return nil, err
	}
	templates := make([]CompactKbuildCommandTemplate, 0, len(selections))
	for _, selection := range selections {
		if strings.TrimSpace(selection.Text) != "" {
			templates = append(templates, selection)
		}
	}
	return templates, nil
}

// ReadCompactKbuildCommandSourceScripts finds shell programs in one evaluated
// command without assigning policy to the cmd_<name> which selected it. A
// program is either a source-tree file whose contents declare a shell
// interpreter, or the first source-tree operand of the target's configured
// CONFIG_SHELL command. Generated and ambient scripts are deliberately
// excluded; filenames do not determine the interpreter contract.
func ReadCompactKbuildCommandSourceScripts(
	profile CompactKbuildProfile,
	target, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	command string,
) ([]CompactKbuildSourceScript, error) {
	return readCompactKbuildCommandSourceScripts(profile, target, stem, normal, orderOnly, injected, command, true)
}

// ReadCompactKbuildCommandSourceScriptsForMakeTarget preserves lexical target
// lookup and automatic-variable identities while discovering immutable source
// programs in a concrete command.
func ReadCompactKbuildCommandSourceScriptsForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	command string,
) ([]CompactKbuildSourceScript, error) {
	return readCompactKbuildCommandSourceScriptsForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		injected, command, true,
	)
}

// ReadCompactKbuildCommandSourceScriptsSymbolic performs source-script
// discovery without reducing compiler probe atoms in the surrounding target
// environment.
func ReadCompactKbuildCommandSourceScriptsSymbolic(
	profile CompactKbuildProfile,
	target, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	command string,
) ([]CompactKbuildSourceScript, error) {
	return readCompactKbuildCommandSourceScripts(profile, target, stem, normal, orderOnly, injected, command, false)
}

// ReadCompactKbuildCommandSourceScriptsSymbolicForMakeTarget is the
// probe-preserving counterpart of the lexical concrete script discovery API.
func ReadCompactKbuildCommandSourceScriptsSymbolicForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	command string,
) ([]CompactKbuildSourceScript, error) {
	return readCompactKbuildCommandSourceScriptsForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		injected, command, false,
	)
}

func readCompactKbuildCommandSourceScripts(
	profile CompactKbuildProfile,
	target, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	command string,
	resolveSymbolic bool,
) ([]CompactKbuildSourceScript, error) {
	return readCompactKbuildCommandSourceScriptsForMakeTarget(
		profile, target, target, target, stem, normal, orderOnly, injected, command, resolveSymbolic,
	)
}

func readCompactKbuildCommandSourceScriptsForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	command string,
	resolveSymbolic bool,
) ([]CompactKbuildSourceScript, error) {
	values, err := evaluateCompactKbuildTargetForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		injected, resolveSymbolic, "CONFIG_SHELL",
	)
	if err != nil {
		return nil, err
	}
	configuredShell := kbuildFields(values["CONFIG_SHELL"])
	commands, err := compactKbuildSourceScriptCommandFields(command)
	if err != nil {
		return nil, fmt.Errorf("inspect Kbuild source-script command for target %q: %w", target, err)
	}
	seen := map[string]bool{}
	scripts := []CompactKbuildSourceScript{}
	for _, fields := range commands {
		path, ok, programErr := compactKbuildSourceScriptProgram(profile, fields, configuredShell)
		if programErr != nil {
			return nil, fmt.Errorf("inspect selected Kbuild source script for target %q: %w", target, programErr)
		}
		if !ok || seen[path] {
			continue
		}
		content, err := readCompactKbuildProfileSource(profile, path)
		if err != nil {
			return nil, fmt.Errorf("read selected Kbuild source script %q for target %q: %w", path, target, err)
		}
		seen[path] = true
		scripts = append(scripts, CompactKbuildSourceScript{Path: path, Content: string(content)})
	}
	return scripts, nil
}

// compactKbuildSourceScriptCommandFields retains only argv words while
// splitting shell command connectors. Redirections stay attached to their
// command so their operands cannot be mistaken for executable programs.
func compactKbuildSourceScriptCommandFields(command string) ([][]string, error) {
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		return nil, err
	}
	commands := [][]string{}
	fields := []string{}
	finish := func() {
		if len(fields) != 0 {
			commands = append(commands, fields)
			fields = nil
		}
	}
	for _, token := range tokens {
		if token.operator {
			switch token.value {
			case ";", "&&", "||", "|", "&", "(", ")":
				finish()
			default:
				fields = append(fields, token.value)
			}
			continue
		}
		fields = append(fields, strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$"))
	}
	finish()
	return commands, nil
}

func compactKbuildSourceScriptProgram(
	profile CompactKbuildProfile,
	fields []string,
	configuredShell []string,
) (string, bool, error) {
	program := 0
	for program < len(fields) {
		name, _, assignment := strings.Cut(fields[program], "=")
		if !assignment || !validKbuildCommandEnvironmentName(name) {
			break
		}
		program++
	}
	if program == len(fields) {
		return "", false, nil
	}
	fields = slices.Clone(fields[program:])
	fields[0] = strings.TrimLeft(fields[0], "+@-")
	if path, source, pathLike := compactKbuildProfileCommandPath(profile, fields[0]); pathLike && source && compactKbuildProfileSourceUsesShell(profile, path) && compactKbuildProfileSourcePathExists(profile, path) {
		return path, true, nil
	}
	selectedShell := len(configuredShell) != 0 && fields[0] == configuredShell[0] &&
		len(fields) >= len(configuredShell) && slices.Equal(fields[1:len(configuredShell)], configuredShell[1:])
	defaultShell := len(configuredShell) == 0 && fields[0] == "sh"
	if selectedShell || defaultShell {
		invocation := compactKbuildShellArguments(fields[1:])
		if invocation.mode != compactKbuildShellModeFile || invocation.scriptIndex < 0 {
			return "", false, nil
		}
		candidate := fields[invocation.scriptIndex+1]
		path, source, pathLike := compactKbuildProfileCommandPath(profile, candidate)
		if !pathLike || !source || !compactKbuildProfileSourcePathExists(profile, path) {
			return "", false, nil
		}
		return path, true, nil
	}
	match, matched, err := compactKbuildProfileSourceInterpreterCommand(profile, fields[0], fields[1:])
	if err != nil || !matched {
		return "", false, err
	}
	return match.scriptPath, true, nil
}

func readCompactKbuildProfileSource(profile CompactKbuildProfile, sourcePath string) ([]byte, error) {
	sourcePath = canonicalKbuildRulePath(sourcePath)
	if err := validatePlanRelativePath("Kbuild source script", sourcePath); err != nil {
		return nil, err
	}
	runtime := compactKbuildPlannerRuntimeForProfile(profile)
	if runtime.sourceRoot == "" {
		return nil, fmt.Errorf("Kbuild profile %q has no source root", profile.Name)
	}
	// Keep the declared source-root spelling here. Bazel materializes declared
	// source trees as symlink forests in local sandboxes, so resolving an
	// individual file can legitimately leave the lexical tree even though the
	// canonical relative path above remains a declared input. Traversal is
	// rejected before this join by validatePlanRelativePath.
	filename := filepath.Join(filepath.Clean(runtime.sourceRoot), filepath.FromSlash(sourcePath))
	info, err := os.Stat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("selected source script %q is not a regular file", sourcePath)
	}
	return os.ReadFile(filename)
}
