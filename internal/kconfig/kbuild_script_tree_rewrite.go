package kconfig

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

type compactKbuildScriptPathReplacement struct {
	from string
	to   string
}

// compactKbuildScriptTreeNames returns the exact typed tree capabilities used
// by an evaluated script. Other shell parameter expansions remain ordinary
// script text and are deliberately ignored.
func compactKbuildScriptTreeNames(script string) ([]string, error) {
	seen := map[string]bool{}
	for _, match := range actionRecipePlaceholder.FindAllStringSubmatch(script, -1) {
		if len(match) != 3 || match[1] != "tree" {
			continue
		}
		if err := validatePlanName("Kbuild script input tree", match[2]); err != nil {
			return nil, err
		}
		seen[match[2]] = true
	}
	if strings.Contains(script, "${tree:") {
		// Every well-formed tree marker is consumed by the expression above.
		// Retaining the prefix therefore indicates a malformed or unterminated
		// capability rather than an ordinary shell variable.
		clean := script
		for name := range seen {
			clean = strings.ReplaceAll(clean, "${tree:"+name+"}", "")
		}
		if strings.Contains(clean, "${tree:") {
			return nil, fmt.Errorf("evaluated script contains a malformed tree binding")
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// rewriteCompactKbuildScriptDeclaredTreeInputs replaces a complete
// ${tree:NAME}/path reference only when path is an exact declared prerequisite
// staged in the script's private working directory. Prefix matches are left
// untouched so a declared `vmlinux` cannot authorize `vmlinux.debug` or a
// descendant path.
func rewriteCompactKbuildScriptDeclaredTreeInputs(
	script string,
	inputs []compactKbuildRuleInput,
) (string, error) {
	trees := make([]string, 0, len(LinuxKernelPlanTrees)+1)
	for tree := range LinuxKernelPlanTrees {
		trees = append(trees, tree)
	}
	trees = append(trees, "kernel")
	sort.Strings(trees)

	replacements := []compactKbuildScriptPathReplacement{}
	seen := map[string]bool{}
	for _, input := range inputs {
		inputPath := canonicalKbuildRulePath(input.path)
		if inputPath == "" {
			continue
		}
		if err := validatePlanRelativePath("Kbuild script prerequisite", inputPath); err != nil {
			return "", err
		}
		for _, tree := range trees {
			from := "${tree:" + tree + "}/" + inputPath
			if !seen[from] {
				seen[from] = true
				replacements = append(replacements, compactKbuildScriptPathReplacement{from: from, to: inputPath})
			}
		}
	}
	sort.Slice(replacements, func(i, j int) bool { return len(replacements[i].from) > len(replacements[j].from) })
	for _, replacement := range replacements {
		script = replaceExactKbuildScriptPath(script, replacement.from, replacement.to)
	}
	return script, nil
}

// rewriteCompactKbuildScriptInvocationInputs preserves the path semantics of
// the Make process which produced an evaluated recipe. Hermetic script
// fallbacks execute at the root of their private staged tree, while a
// recursive `make -C directory` may have spelled immutable or generated
// prerequisites relative to directory. Rewrite only a complete spelling which
// resolves to an exact declared input; the canonical replacement is the path
// at which that input is staged below the private root.
func rewriteCompactKbuildScriptInvocationInputs(
	script string,
	profile CompactKbuildProfile,
	inputs []compactKbuildRuleInput,
) (string, error) {
	location, ok := CompactKbuildProfileInvocationLocation(profile)
	invocationDirectory := location.Directory
	invocationDirectory = canonicalKbuildRulePath(invocationDirectory)
	if !ok || invocationDirectory == "" {
		return script, nil
	}
	if err := validatePlanRelativePath("Kbuild invocation directory", invocationDirectory); err != nil {
		return "", err
	}

	replacements := []compactKbuildScriptPathReplacement{}
	seen := map[string]bool{}
	appendReplacement := func(from, to string) {
		if from == "" || from == "." || from == to || seen[from] {
			return
		}
		seen[from] = true
		replacements = append(replacements, compactKbuildScriptPathReplacement{from: from, to: to})
	}
	for _, input := range inputs {
		inputPath := canonicalKbuildRulePath(input.path)
		if inputPath == "" {
			continue
		}
		if err := validatePlanRelativePath("Kbuild script prerequisite", inputPath); err != nil {
			return "", err
		}
		relative, err := filepath.Rel(
			filepath.FromSlash(invocationDirectory),
			filepath.FromSlash(inputPath),
		)
		if err != nil {
			return "", fmt.Errorf("resolve Kbuild prerequisite %q from invocation directory %q: %w", inputPath, invocationDirectory, err)
		}
		relative = filepath.ToSlash(relative)
		appendReplacement(relative, inputPath)
		if relative != ".." && !strings.HasPrefix(relative, "../") && !strings.HasPrefix(relative, "./") {
			appendReplacement("./"+relative, inputPath)
		}
	}
	sort.Slice(replacements, func(i, j int) bool { return len(replacements[i].from) > len(replacements[j].from) })
	return replaceExactKbuildScriptRelativePaths(script, replacements)
}

// rewriteCompactKbuildCommandInvocationInputs applies the same exact cwd
// projection to argv-lowered recipes. The shell lexer has already reduced
// quoting here, so a complete argument or the value after an option's '=' can
// be replaced without reinterpreting command syntax.
func rewriteCompactKbuildCommandInvocationInputs(
	profile CompactKbuildProfile,
	command compactKbuildRecipeCommand,
	inputs []compactKbuildRuleInput,
) compactKbuildRecipeCommand {
	command.arguments = slices.Clone(command.arguments)
	command.environment = maps.Clone(command.environment)
	rewrite := func(value string) string {
		prefix := ""
		candidate := value
		if index := strings.IndexByte(value, '='); index >= 0 {
			prefix = value[:index+1]
			candidate = value[index+1:]
		}
		if candidate == "" || strings.ContainsAny(candidate, "$`\x00\r\n") {
			return value
		}
		resolved, ok := compactKbuildProfileInvocationRelativePath(profile, candidate)
		resolved = canonicalKbuildRulePath(resolved)
		if !ok || resolved == "" || validatePlanRelativePath("Kbuild command input", resolved) != nil {
			return value
		}
		for _, input := range inputs {
			if canonicalKbuildRulePath(input.path) == resolved {
				return prefix + resolved
			}
		}
		return value
	}
	for index, argument := range command.arguments {
		command.arguments[index] = rewrite(argument)
	}
	command.stdin = rewrite(command.stdin)
	for name, value := range command.environment {
		command.environment[name] = rewrite(value)
	}
	return command
}

// rewriteCompactKbuildCommandAutomaticPaths projects complete argv fields
// produced by automatic variables back onto their canonical graph identities
// after Make evaluation has finished. Evaluation itself must use makeWord so
// textual functions observe exact source/object/relative spellings; action
// binding must use graphPath so declaration-local names such as %.c and
// $(OUTPUT)%.o address the correct staged input and output.
func rewriteCompactKbuildCommandAutomaticPaths(
	profile CompactKbuildProfile,
	target string,
	match compactKbuildRuleMatch,
	command compactKbuildRecipeCommand,
) (compactKbuildRecipeCommand, error) {
	if !match.resolved {
		return command, nil
	}
	command.arguments = slices.Clone(command.arguments)
	command.environment = maps.Clone(command.environment)
	context, err := evaluatedKbuildSelectedTargetMakeContext(profile, target, &match)
	if err != nil {
		return compactKbuildRecipeCommand{}, err
	}
	replacements := map[string]string{}
	appendPath := func(value compactKbuildEvaluatedPath) error {
		makeWord := compactKbuildProfileCanonicalRecipeText(profile, value.makeWord)
		if makeWord == "" || value.graphPath == "" {
			return nil
		}
		if previous := replacements[makeWord]; previous != "" && previous != value.graphPath {
			return fmt.Errorf(
				"Kbuild automatic word %q maps to conflicting graph paths %q and %q",
				makeWord, previous, value.graphPath,
			)
		}
		replacements[makeWord] = value.graphPath
		return nil
	}
	_, compiler := compactKbuildCommandCompilerRole(command)
	if !compiler {
		if err := appendPath(context.target); err != nil {
			return compactKbuildRecipeCommand{}, err
		}
	}
	for _, prerequisite := range append(append([]compactKbuildEvaluatedPath(nil), context.normal...), context.orderOnly...) {
		if err := appendPath(prerequisite); err != nil {
			return compactKbuildRecipeCommand{}, err
		}
	}
	rewrite := func(value string) string {
		if replacement := replacements[value]; replacement != "" {
			return replacement
		}
		if index := strings.IndexByte(value, '='); index >= 0 {
			if replacement := replacements[value[index+1:]]; replacement != "" {
				return value[:index+1] + replacement
			}
		}
		return value
	}
	// A command head is executable provenance, not an automatic-variable
	// artifact operand. It can have the same bytes as a prerequisite because
	// Kbuild deliberately lists generated tools in foo-deps. Projecting that
	// rooted program to the prerequisite's logical path loses its source/object
	// tree identity and can make it relative to an external module's cwd.
	command.stdin = rewrite(command.stdin)
	command.stdout = rewrite(command.stdout)
	for index, argument := range command.arguments {
		command.arguments[index] = rewrite(argument)
	}
	for name, value := range command.environment {
		command.environment[name] = rewrite(value)
	}
	return command, nil
}

// rewriteCompactKbuildCommandRootedRuleOutputs removes an explicit writable
// tree root only after the selected command's output analyzer has consumed its
// provenance. This covers automatic-target data embedded in compiler options
// such as -Wp,...,-MT,$@ and source transformations which name grouped peers,
// without using equality of an unrooted source word as authority.
func rewriteCompactKbuildCommandRootedRuleOutputs(
	target string,
	match compactKbuildRuleMatch,
	command compactKbuildRecipeCommand,
) compactKbuildRecipeCommand {
	replacements := []compactKbuildScriptPathReplacement{}
	for _, output := range compactKbuildRecipeRuleOutputs(target, match) {
		for _, root := range []string{
			"${tree:prep}", "${tree:host}", "${tree:bootstrap}", "${tree:prehost}",
			"${work:root}", "__LINUX_BZL_OBJECT_TREE__",
		} {
			replacements = append(replacements, compactKbuildScriptPathReplacement{
				from: root + "/" + output,
				to:   output,
			})
		}
	}
	rewrite := func(value string) string {
		for _, replacement := range replacements {
			value = replaceExactKbuildScriptPath(value, replacement.from, replacement.to)
		}
		return value
	}
	command.arguments = slices.Clone(command.arguments)
	command.environment = maps.Clone(command.environment)
	command.stdin = rewrite(command.stdin)
	command.stdout = rewrite(command.stdout)
	for index, argument := range command.arguments {
		command.arguments[index] = rewrite(argument)
	}
	for name, value := range command.environment {
		command.environment[name] = rewrite(value)
	}
	return command
}

// rewriteCompactKbuildEvaluatedAutomaticTreePaths removes the temporary tree
// namespace from complete automatic-variable paths after Make's textual
// functions have finished. Injected $(obj)/$(src) values that merely share the
// same directory remain rooted; only the concrete $@/$</$^/$+/$| identities
// selected by the rule context are projected back to their logical graph path.
// This also handles automatic paths embedded in structured argv fields (for
// example rustc's --emit=dep-info=...,obj=$@) and in hermetic script fallbacks.
func rewriteCompactKbuildEvaluatedAutomaticTreePaths(
	profile CompactKbuildProfile,
	target string,
	match compactKbuildRuleMatch,
	inputs []compactKbuildRuleInput,
	value string,
) (string, error) {
	if !match.resolved || value == "" {
		return value, nil
	}
	context, err := evaluatedKbuildSelectedTargetMakeContext(profile, target, &match)
	if err != nil {
		return "", err
	}
	replacements := []compactKbuildScriptPathReplacement{}
	seen := map[string]bool{}
	appendPath := func(path compactKbuildEvaluatedPath, target bool) error {
		graphPath := compactKbuildGraphTargetPath(path.graphPath)
		if graphPath == "" || graphPath == "FORCE" {
			return nil
		}
		marker := "__LINUX_BZL_OBJECT_TREE__"
		if !target {
			var err error
			marker, err = compactKbuildAutomaticGraphPathMarker(profile, graphPath, inputs)
			if err != nil {
				return err
			}
		}
		roots := []string{"__LINUX_BZL_OBJECT_TREE__", "${tree:prep}", "${tree:host}", "${tree:bootstrap}", "${tree:prehost}", "${work:root}"}
		if marker == "__LINUX_BZL_SOURCE_TREE__" {
			roots = []string{"__LINUX_BZL_SOURCE_TREE__", "${tree:kernel}"}
		}
		replacement := graphPath
		if marker == "__LINUX_BZL_SOURCE_TREE__" {
			// Preserve immutable-source provenance through argv parsing. The
			// command-level automatic-path rewrite can project the execution
			// operand onto its graph identity after source-script discovery has
			// retained this typed spelling.
			replacement = "${tree:kernel}/" + graphPath
		}
		spellings := []string{graphPath}
		makeWord := strings.TrimSpace(filepathToSlash(path.makeWord))
		for _, namespace := range []string{"__LINUX_BZL_SOURCE_TREE__", "__LINUX_BZL_OBJECT_TREE__"} {
			makeWord = strings.TrimPrefix(makeWord, namespace+"/")
		}
		for strings.HasPrefix(makeWord, "./") {
			makeWord = strings.TrimPrefix(makeWord, "./")
		}
		if makeWord != "" && compactKbuildGraphTargetPath(makeWord) == graphPath && makeWord != graphPath {
			spellings = append(spellings, makeWord)
		}
		for _, root := range roots {
			for _, spelling := range spellings {
				from := root + "/" + spelling
				if from == replacement || seen[from] {
					continue
				}
				seen[from] = true
				replacements = append(replacements, compactKbuildScriptPathReplacement{from: from, to: replacement})
			}
		}
		return nil
	}
	// Keep the automatic target rooted until command-level lowering. Compiler
	// output analysis needs that explicit object-tree provenance to distinguish
	// `$@` from identical source-authored relative bytes. Ordinary commands
	// project the complete target word after argv parsing; compiler output
	// operands are projected by the compiler analyzer itself.
	for _, prerequisite := range context.normal {
		if err := appendPath(prerequisite, false); err != nil {
			return "", err
		}
	}
	for _, prerequisite := range context.orderOnly {
		if err := appendPath(prerequisite, false); err != nil {
			return "", err
		}
	}
	sort.Slice(replacements, func(i, j int) bool { return len(replacements[i].from) > len(replacements[j].from) })

	// The template rewrite precedes argv lowering and therefore cannot infer
	// automatic-variable provenance merely from equal bytes. Discover command
	// heads from the evaluated shell grammar and leave those spans rooted while
	// projecting arguments, redirections, and environment values. If the shell
	// grammar is undecidable, retaining every rooted spelling is the safe result:
	// later script lowering still has exact tree provenance, whereas a guessed
	// projection could execute a different file.
	commands, commandErr := compactKbuildCompoundProgramCommands(value)
	if commandErr != nil {
		return value, nil
	}
	protected := make([]compactKbuildScriptPathProtection, 0, len(commands))
	for _, command := range commands {
		if command.programStart < 0 || command.programEnd <= command.programStart || command.programEnd > len(value) {
			return "", fmt.Errorf(
				"evaluated command program %q has invalid source span [%d,%d)",
				command.program, command.programStart, command.programEnd,
			)
		}
		protected = append(protected, compactKbuildScriptPathProtection{
			start: command.programStart,
			end:   command.programEnd,
		})
	}
	return replaceCompactKbuildScriptPathsOutside(value, replacements, protected)
}

type compactKbuildScriptPathProtection struct {
	start int
	end   int
}

func replaceCompactKbuildScriptPathsOutside(
	value string,
	replacements []compactKbuildScriptPathReplacement,
	protected []compactKbuildScriptPathProtection,
) (string, error) {
	sort.Slice(protected, func(i, j int) bool {
		if protected[i].start != protected[j].start {
			return protected[i].start < protected[j].start
		}
		return protected[i].end < protected[j].end
	})
	rewrite := func(segment string) string {
		for _, replacement := range replacements {
			segment = replaceExactKbuildScriptPath(segment, replacement.from, replacement.to)
		}
		return segment
	}
	var out strings.Builder
	cursor := 0
	for _, span := range protected {
		if span.start < cursor || span.end > len(value) || span.end <= span.start {
			return "", fmt.Errorf("overlapping or invalid protected command span [%d,%d)", span.start, span.end)
		}
		out.WriteString(rewrite(value[cursor:span.start]))
		out.WriteString(value[span.start:span.end])
		cursor = span.end
	}
	out.WriteString(rewrite(value[cursor:]))
	return out.String(), nil
}

func replaceExactKbuildScriptPath(value, from, to string) string {
	if from == "" || !strings.Contains(value, from) {
		return value
	}
	var out strings.Builder
	start := 0
	for {
		relative := strings.Index(value[start:], from)
		if relative < 0 {
			out.WriteString(value[start:])
			break
		}
		index := start + relative
		end := index + len(from)
		out.WriteString(value[start:index])
		if end < len(value) && isKbuildScriptPathContinuation(value[end]) {
			out.WriteString(from)
		} else {
			out.WriteString(to)
		}
		start = end
	}
	return out.String()
}

func replaceExactKbuildScriptRelativePaths(
	value string,
	replacements []compactKbuildScriptPathReplacement,
) (string, error) {
	lines := strings.SplitAfter(value, "\n")
	pending := []compactKbuildHeredoc{}
	quote := byte(0)
	var out strings.Builder
	for lineIndex := 0; lineIndex < len(lines); lineIndex++ {
		line := lines[lineIndex]
		if len(pending) != 0 {
			document := pending[0]
			candidate := strings.TrimSuffix(line, "\n")
			if document.stripTabs {
				candidate = strings.TrimLeft(candidate, "\t")
			}
			if candidate == document.delimiter {
				pending = pending[1:]
			}
			out.WriteString(line)
			continue
		}

		chunk := line
		var sanitized string
		var documents []compactKbuildHeredoc
		var nextQuote byte
		for {
			var err error
			sanitized, documents, nextQuote, err = compactKbuildSourceScriptCommandLine(chunk, quote)
			if err == nil {
				break
			}
			if !compactKbuildSourceScriptContinuationError(err) || lineIndex+1 >= len(lines) {
				return "", err
			}
			lineIndex++
			chunk += lines[lineIndex]
		}
		quote = nextQuote
		pending = append(pending, documents...)
		// Backslash-newline removal can make the sanitized representation
		// shorter than the source. Such a spelling cannot itself be one complete
		// literal path, so leave this logical line untouched rather than guessing
		// source offsets.
		if len(sanitized) != len(chunk) {
			out.WriteString(chunk)
			continue
		}
		for index := 0; index < len(chunk); {
			matched := false
			for _, replacement := range replacements {
				end := index + len(replacement.from)
				if end > len(chunk) || chunk[index:end] != replacement.from || sanitized[index:end] != replacement.from {
					continue
				}
				if (index > 0 && isKbuildScriptRelativePathContinuation(chunk[index-1])) ||
					(end < len(chunk) && isKbuildScriptRelativePathContinuation(chunk[end])) {
					continue
				}
				out.WriteString(replacement.to)
				index = end
				matched = true
				break
			}
			if matched {
				continue
			}
			out.WriteByte(chunk[index])
			index++
		}
	}
	if quote != 0 {
		return "", fmt.Errorf("unterminated shell quote")
	}
	if len(pending) != 0 {
		return "", fmt.Errorf("unterminated here-document %q", pending[0].delimiter)
	}
	return out.String(), nil
}

func isKbuildScriptRelativePathContinuation(value byte) bool {
	return isKbuildScriptPathContinuation(value) || strings.ContainsRune("$`*?[]{}\\", rune(value))
}

func isKbuildScriptPathContinuation(value byte) bool {
	switch {
	case value >= 'a' && value <= 'z':
		return true
	case value >= 'A' && value <= 'Z':
		return true
	case value >= '0' && value <= '9':
		return true
	}
	return strings.ContainsRune("_./+-", rune(value))
}
