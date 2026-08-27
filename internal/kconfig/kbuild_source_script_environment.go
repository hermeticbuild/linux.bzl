package kconfig

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

const maxCompactKbuildSourceScriptWrapperDepth = 32

// compactKbuildSourceScriptEnvironmentUsage is the source-owned capability
// projection for one immutable shell program. Names are environment variables
// read through bounded shell syntax. This is deliberately not a claim to model
// every getenv performed by arbitrary child programs: role-bearing Make values
// are executable capabilities and must be syntactically visible in the script
// (or a bounded recursive replay) to enter the action. ObservesAll is the
// fail-closed result for shell constructs which explicitly inspect or source
// the environment dynamically.
type compactKbuildSourceScriptEnvironmentUsage struct {
	Names           map[string]bool
	arithmeticNames map[string]bool
	wholeValues     map[string]bool
	valuePaths      map[string]map[string]bool
	programs        map[string]bool
	// argumentVectorUses counts active $*/$@ expansions, while
	// argumentVectorProgramUses counts the subset which forms a complete shell
	// command head. positionalArgumentUses keeps any other special positional
	// expansion conservative. Object-tree observation uses this source syntax to
	// distinguish a compiler argv forwarded as a command from opaque data
	// arguments; it never assigns semantics from a script filename.
	argumentVectorUses        int
	argumentVectorProgramUses int
	positionalArgumentUses    int
	positionalArgumentIndexes map[int]bool
	positionalArgumentDynamic bool
	ObservesAll               bool
}

func (u *compactKbuildSourceScriptEnvironmentUsage) add(name string) {
	if validKbuildCommandEnvironmentName(name) {
		if u.Names == nil {
			u.Names = map[string]bool{}
		}
		u.Names[name] = true
	}
}

func (u *compactKbuildSourceScriptEnvironmentUsage) merge(other compactKbuildSourceScriptEnvironmentUsage) {
	for name := range other.Names {
		u.add(name)
	}
	for name := range other.arithmeticNames {
		if u.arithmeticNames == nil {
			u.arithmeticNames = map[string]bool{}
		}
		u.arithmeticNames[name] = true
	}
	for name := range other.wholeValues {
		u.observeWholeValue(name)
	}
	for name, paths := range other.valuePaths {
		for pathname := range paths {
			u.observeValuePath(name, pathname)
		}
	}
	for program := range other.programs {
		u.addProgram(program)
	}
	u.argumentVectorUses += other.argumentVectorUses
	u.argumentVectorProgramUses += other.argumentVectorProgramUses
	u.positionalArgumentUses += other.positionalArgumentUses
	for index := range other.positionalArgumentIndexes {
		if u.positionalArgumentIndexes == nil {
			u.positionalArgumentIndexes = map[int]bool{}
		}
		u.positionalArgumentIndexes[index] = true
	}
	u.positionalArgumentDynamic = u.positionalArgumentDynamic || other.positionalArgumentDynamic
	u.ObservesAll = u.ObservesAll || other.ObservesAll
}

func (u *compactKbuildSourceScriptEnvironmentUsage) observePositionalArgument(index int) {
	if index <= 0 {
		return
	}
	u.positionalArgumentUses++
	if u.positionalArgumentIndexes == nil {
		u.positionalArgumentIndexes = map[int]bool{}
	}
	u.positionalArgumentIndexes[index] = true
}

func (u *compactKbuildSourceScriptEnvironmentUsage) addArithmetic(name string) {
	u.observeWholeValue(name)
	if validKbuildCommandEnvironmentName(name) {
		if u.arithmeticNames == nil {
			u.arithmeticNames = map[string]bool{}
		}
		u.arithmeticNames[name] = true
	}
}

func (u *compactKbuildSourceScriptEnvironmentUsage) observeWholeValue(name string) {
	u.add(name)
	if !validKbuildCommandEnvironmentName(name) {
		return
	}
	if u.wholeValues == nil {
		u.wholeValues = map[string]bool{}
	}
	u.wholeValues[name] = true
}

func (u *compactKbuildSourceScriptEnvironmentUsage) observeValuePath(name, pathname string) {
	u.add(name)
	pathname = canonicalKbuildRulePath(pathname)
	if !validKbuildCommandEnvironmentName(name) || pathname == "" || pathname == "." || strings.ContainsAny(pathname, "$%") {
		u.observeWholeValue(name)
		return
	}
	if u.valuePaths == nil {
		u.valuePaths = map[string]map[string]bool{}
	}
	if u.valuePaths[name] == nil {
		u.valuePaths[name] = map[string]bool{}
	}
	u.valuePaths[name][pathname] = true
}

func (u *compactKbuildSourceScriptEnvironmentUsage) addProgram(program string) {
	if path.Base(program) != program || validatePlanName("source-script program", program) != nil {
		return
	}
	if u.programs == nil {
		u.programs = map[string]bool{}
	}
	u.programs[program] = true
}

func (u compactKbuildSourceScriptEnvironmentUsage) uses(name string) bool {
	return u.ObservesAll || u.Names[name]
}

type compactKbuildSourceScriptScan struct {
	usage          compactKbuildSourceScriptEnvironmentUsage
	sources        []string
	programSources []string
}

// compactKbuildSourceScriptUsage reads the exact selected source program and
// any statically named immutable files it sources. Dynamic sourcing observes
// all role-bearing environment capabilities, rather than guessing which
// compiler/tool scope the sourced program might consume.
func compactKbuildSourceScriptUsage(
	profile CompactKbuildProfile,
	scriptPath string,
) (compactKbuildSourceScriptEnvironmentUsage, error) {
	usage := compactKbuildSourceScriptEnvironmentUsage{Names: map[string]bool{}}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(current string) error {
		current = canonicalKbuildRulePath(current)
		if visited[current] {
			return nil
		}
		visited[current] = true
		content, err := readCompactKbuildProfileSource(profile, current)
		if err != nil {
			return err
		}
		scan, err := scanCompactKbuildSourceScript(string(content))
		if err != nil {
			return fmt.Errorf("inspect environment use in %q: %w", current, err)
		}
		usage.merge(scan.usage)
		for _, source := range scan.sources {
			resolved, ok := compactKbuildStaticSourcedPath(profile, current, source)
			if !ok {
				usage.ObservesAll = true
				continue
			}
			if err := visit(resolved); err != nil {
				return err
			}
		}
		for _, source := range scan.programSources {
			resolved, ok := compactKbuildStaticSourcedPath(profile, current, source)
			if !ok {
				continue
			}
			if !compactKbuildProfileSourceUsesShell(profile, resolved) {
				continue
			}
			if err := visit(resolved); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(scriptPath); err != nil {
		return compactKbuildSourceScriptEnvironmentUsage{}, err
	}
	return usage, nil
}

// compactKbuildHermeticScriptEnvironmentUsage reports environment observed by
// one evaluated shell fallback and by immutable shell helpers it invokes. It is
// deliberately scope-free: graph discovery has not selected host versus target
// yet, and environment role provenance is what makes that decision.
func compactKbuildHermeticScriptEnvironmentUsage(
	profile CompactKbuildProfile,
	script string,
	commands []compactKbuildRecipeCommand,
) (compactKbuildSourceScriptEnvironmentUsage, error) {
	scan, err := scanCompactKbuildSourceScript(script)
	if err != nil {
		return compactKbuildSourceScriptEnvironmentUsage{}, err
	}
	usage := scan.usage
	seen := map[string]bool{}
	for _, command := range commands {
		candidates := append([]string{command.program}, command.arguments...)
		for _, candidate := range candidates {
			pathname, source, pathLike := compactKbuildProfileCommandPath(profile, candidate)
			if !pathLike || !source || seen[pathname] || !compactKbuildProfileSourceUsesShell(profile, pathname) {
				continue
			}
			seen[pathname] = true
			child, childErr := compactKbuildSourceScriptUsage(profile, pathname)
			if childErr != nil {
				return compactKbuildSourceScriptEnvironmentUsage{}, childErr
			}
			usage.merge(child)
		}
	}
	return usage, nil
}

func compactKbuildStaticSourcedPath(profile CompactKbuildProfile, current, source string) (string, bool) {
	if source == "" || strings.ContainsAny(source, "$`") || path.IsAbs(source) {
		return "", false
	}
	candidates := []string{canonicalKbuildRulePath(source)}
	if directory := path.Dir(current); directory != "." {
		candidates = append(candidates, canonicalKbuildRulePath(path.Join(directory, source)))
	}
	candidates = slices.Compact(candidates)
	for _, candidate := range candidates {
		if candidate != "" && compactKbuildProfileSourcePathExists(profile, candidate) {
			return candidate, true
		}
	}
	return "", false
}

type compactKbuildHeredoc struct {
	delimiter string
	quoted    bool
	stripTabs bool
}

// prepareCompactKbuildSourceScript masks comments and here-document payloads
// before ordinary shell lexing. Unquoted here-documents remain active for
// parameter discovery, but never become command text; quoted ones are inert.
func prepareCompactKbuildSourceScript(content string) (string, string, error) {
	lines := strings.SplitAfter(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var expansions, commands strings.Builder
	pending := []compactKbuildHeredoc{}
	quote := byte(0)
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
				expansions.WriteString(strings.Repeat(" ", len(strings.TrimSuffix(line, "\n"))))
				commands.WriteString(strings.Repeat(" ", len(strings.TrimSuffix(line, "\n"))))
			} else if document.quoted {
				expansions.WriteString(strings.Repeat(" ", len(strings.TrimSuffix(line, "\n"))))
				commands.WriteString(strings.Repeat(" ", len(strings.TrimSuffix(line, "\n"))))
			} else {
				active := strings.NewReplacer("'", " ", "\"", " ", "#", " ").Replace(strings.TrimSuffix(line, "\n"))
				expansions.WriteString(active)
				commands.WriteString(strings.Repeat(" ", len(active)))
			}
			if strings.HasSuffix(line, "\n") {
				expansions.WriteByte('\n')
				commands.WriteString(";\n")
			}
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
				return "", "", err
			}
			lineIndex++
			chunk += lines[lineIndex]
		}
		quote = nextQuote
		expansions.WriteString(sanitized)
		if strings.HasSuffix(sanitized, "\n") && quote == 0 {
			commands.WriteString(strings.TrimSuffix(sanitized, "\n"))
			commands.WriteString(";\n")
		} else {
			commands.WriteString(sanitized)
		}
		pending = append(pending, documents...)
	}
	if quote != 0 {
		return "", "", fmt.Errorf("unterminated shell quote")
	}
	if len(pending) != 0 {
		return "", "", fmt.Errorf("unterminated here-document %q", pending[0].delimiter)
	}
	return expansions.String(), commands.String(), nil
}

func compactKbuildSourceScriptContinuationError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return message == "unterminated backtick command substitution" ||
		message == "unterminated shell command substitution" ||
		message == "unterminated shell arithmetic expansion" ||
		message == "unterminated shell parameter expansion"
}

func compactKbuildSourceScriptCommandLine(
	line string,
	initialQuote byte,
) (string, []compactKbuildHeredoc, byte, error) {
	var out strings.Builder
	documents := []compactKbuildHeredoc{}
	quote := initialQuote
	wordStart := true
	for index := 0; index < len(line); {
		character := line[index]
		if character == '\\' && quote != '\'' {
			// An unquoted or double-quoted backslash-newline pair is removed by
			// the shell before tokenization. Remove it here too so a direct
			// environment name split across physical lines remains one name.
			if index+1 < len(line) && line[index+1] == '\n' {
				index += 2
				continue
			}
			out.WriteByte(character)
			if index+1 < len(line) {
				out.WriteByte(line[index+1])
				index += 2
				wordStart = false
				continue
			}
			index++
			continue
		}
		if character == '`' && quote != '\'' {
			end, err := compactKbuildBacktickCommandEnd(line, index+1)
			if err != nil {
				return "", nil, quote, err
			}
			// Quotes inside a legacy command substitution belong to its nested
			// shell parse and must not open or close the surrounding quote. Keep
			// the bytes for the recursive capability scan while advancing the
			// outer quote machine across the complete substitution.
			out.WriteString(line[index:end])
			index = end
			wordStart = false
			continue
		}
		if character == '$' && quote != '\'' && index+1 < len(line) {
			end := 0
			var err error
			switch line[index+1] {
			case '(':
				if index+2 < len(line) && line[index+2] == '(' {
					end, err = compactKbuildArithmeticEnd(line, index+3)
				} else {
					end, err = compactKbuildCommandSubstitutionEnd(line, index+2)
				}
			case '{':
				end, err = compactKbuildBracedExpansionEnd(line, index+2)
			}
			if err != nil {
				return "", nil, quote, err
			}
			if end != 0 {
				// Nested quotes belong to the expansion's own parse. Preserve the
				// full expansion for later parameter/command inspection without
				// letting those quotes mutate the surrounding shell quote state.
				out.WriteString(line[index:end])
				index = end
				wordStart = false
				continue
			}
		}
		if quote != 0 {
			out.WriteByte(character)
			if character == quote {
				quote = 0
			}
			index++
			wordStart = false
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			out.WriteByte(character)
			index++
			wordStart = false
			continue
		}
		if character == '#' && wordStart {
			for index < len(line) && line[index] != '\n' {
				out.WriteByte(' ')
				index++
			}
			continue
		}
		if character == '<' && index+1 < len(line) && line[index+1] == '<' && (index+2 == len(line) || line[index+2] != '<') {
			out.WriteString("<<")
			index += 2
			document := compactKbuildHeredoc{}
			if index < len(line) && line[index] == '-' {
				document.stripTabs = true
				out.WriteByte('-')
				index++
			}
			for index < len(line) && (line[index] == ' ' || line[index] == '\t') {
				out.WriteByte(line[index])
				index++
			}
			var delimiter strings.Builder
			for index < len(line) && !strings.ContainsRune(" \t\r\n;|&<>()", rune(line[index])) {
				if line[index] == '$' || line[index] == '`' {
					return "", nil, quote, fmt.Errorf("dynamic here-document delimiter")
				}
				if line[index] == '\'' || line[index] == '"' {
					document.quoted = true
					closing := line[index]
					out.WriteByte(line[index])
					index++
					for index < len(line) && line[index] != closing {
						delimiter.WriteByte(line[index])
						out.WriteByte(line[index])
						index++
					}
					if index == len(line) {
						return "", nil, quote, fmt.Errorf("unterminated quoted here-document delimiter")
					}
					out.WriteByte(line[index])
					index++
					continue
				}
				if line[index] == '\\' {
					document.quoted = true
					out.WriteByte(line[index])
					index++
					if index == len(line) {
						return "", nil, quote, fmt.Errorf("trailing escape in here-document delimiter")
					}
				}
				delimiter.WriteByte(line[index])
				out.WriteByte(line[index])
				index++
			}
			if delimiter.Len() == 0 {
				return "", nil, quote, fmt.Errorf("empty here-document delimiter")
			}
			document.delimiter = delimiter.String()
			documents = append(documents, document)
			wordStart = false
			continue
		}
		out.WriteByte(character)
		index++
		wordStart = strings.ContainsRune(" \t\r\n;|&()", rune(character))
	}
	return out.String(), documents, quote, nil
}

func compactKbuildBacktickCommandEnd(text string, start int) (int, error) {
	for index := start; index < len(text); index++ {
		if text[index] == '\\' {
			index++
			continue
		}
		if text[index] == '`' {
			return index + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated backtick command substitution")
}

func scanCompactKbuildSourceScript(content string) (compactKbuildSourceScriptScan, error) {
	expansions, commands, err := prepareCompactKbuildSourceScript(content)
	if err != nil {
		return compactKbuildSourceScriptScan{}, err
	}
	scan := compactKbuildSourceScriptScan{
		usage: compactKbuildSourceScriptEnvironmentUsage{Names: map[string]bool{}},
	}
	if err := scanCompactKbuildShellParameters(expansions, &scan.usage); err != nil {
		return compactKbuildSourceScriptScan{}, err
	}
	if err := scanCompactKbuildBacktickCommands(expansions, &scan); err != nil {
		return compactKbuildSourceScriptScan{}, err
	}
	if err := scanCompactKbuildDollarCommandSubstitutions(expansions, &scan); err != nil {
		return compactKbuildSourceScriptScan{}, err
	}
	commands, err = maskCompactKbuildSourceScriptSubstitutions(commands)
	if err != nil {
		return compactKbuildSourceScriptScan{}, err
	}
	inspectCompactKbuildSourceScriptCommands(commands, &scan)
	slices.Sort(scan.sources)
	scan.sources = slices.Compact(scan.sources)
	slices.Sort(scan.programSources)
	scan.programSources = slices.Compact(scan.programSources)
	return scan, nil
}

// scanCompactKbuildBacktickCommands applies the same command and parameter
// inspection to active legacy command substitutions as to $(...). Parameter
// scanning alone is insufficient: `printenv HOSTCC` names HOSTCC without a
// dollar expansion. Escaped nested backticks are rare and ambiguous enough
// that the bounded scanner fails closed instead of guessing their shell parse.
func scanCompactKbuildBacktickCommands(text string, scan *compactKbuildSourceScriptScan) error {
	quote := byte(0)
	for index := 0; index < len(text); {
		character := text[index]
		if character == '\\' && quote != '\'' {
			index += 2
			continue
		}
		if quote == '\'' {
			if character == '\'' {
				quote = 0
			}
			index++
			continue
		}
		if character == '\'' && quote == 0 {
			quote = '\''
			index++
			continue
		}
		if character == '"' {
			if quote == '"' {
				quote = 0
			} else if quote == 0 {
				quote = '"'
			}
			index++
			continue
		}
		if character != '`' {
			index++
			continue
		}
		end := index + 1
		for end < len(text) {
			if text[end] == '\\' {
				if end+1 < len(text) && text[end+1] == '`' {
					scan.usage.ObservesAll = true
				}
				end += 2
				continue
			}
			if text[end] == '`' {
				break
			}
			end++
		}
		commandEnd, err := compactKbuildBacktickCommandEnd(text, index+1)
		if err != nil {
			return err
		}
		nested, err := scanCompactKbuildSourceScript(text[index+1 : commandEnd-1])
		if err != nil {
			scan.usage.ObservesAll = true
		} else {
			scan.usage.merge(nested.usage)
			scan.sources = append(scan.sources, nested.sources...)
			scan.programSources = append(scan.programSources, nested.programSources...)
		}
		index = commandEnd
	}
	return nil
}

// scanCompactKbuildDollarCommandSubstitutions recursively inspects the command
// bodies of active $(...) expansions. The ordinary parameter pass sees dollar
// reads inside these bodies, but direct forms such as $(printenv HOSTCC) and
// immutable child scripts also need command-level inspection.
func scanCompactKbuildDollarCommandSubstitutions(text string, scan *compactKbuildSourceScriptScan) error {
	quote := byte(0)
	for index := 0; index < len(text); {
		character := text[index]
		if character == '\\' && quote != '\'' {
			index += 2
			continue
		}
		if quote == '\'' {
			if character == '\'' {
				quote = 0
			}
			index++
			continue
		}
		if character == '\'' && quote == 0 {
			quote = '\''
			index++
			continue
		}
		if character == '"' {
			if quote == '"' {
				quote = 0
			} else if quote == 0 {
				quote = '"'
			}
			index++
			continue
		}
		if character == '`' {
			end, err := compactKbuildBacktickCommandEnd(text, index+1)
			if err != nil {
				return err
			}
			index = end
			continue
		}
		if character != '$' || index+1 >= len(text) {
			index++
			continue
		}
		switch text[index+1] {
		case '(':
			if index+2 < len(text) && text[index+2] == '(' {
				end, err := compactKbuildArithmeticEnd(text, index+3)
				if err != nil {
					return err
				}
				if err := scanCompactKbuildDollarCommandSubstitutions(text[index+3:end-2], scan); err != nil {
					return err
				}
				index = end
				continue
			}
			end, err := compactKbuildCommandSubstitutionEnd(text, index+2)
			if err != nil {
				return err
			}
			nested, nestedErr := scanCompactKbuildSourceScript(text[index+2 : end-1])
			if nestedErr != nil {
				scan.usage.ObservesAll = true
			} else {
				scan.usage.merge(nested.usage)
				scan.sources = append(scan.sources, nested.sources...)
				scan.programSources = append(scan.programSources, nested.programSources...)
			}
			index = end
			continue
		case '{':
			end, err := compactKbuildBracedExpansionEnd(text, index+2)
			if err != nil {
				return err
			}
			if err := scanCompactKbuildDollarCommandSubstitutions(text[index+2:end-1], scan); err != nil {
				return err
			}
			index = end
			continue
		}
		index++
	}
	return nil
}

// maskCompactKbuildSourceScriptSubstitutions removes nested shell programs
// from the outer command-token stream after they have been inspected above.
// Their quotes, pipes, and parentheses belong to the nested parse and must not
// be interpreted as syntax of the containing command.
func maskCompactKbuildSourceScriptSubstitutions(text string) (string, error) {
	const placeholder = "__LINUX_BZL_SUBSTITUTION__"
	var out strings.Builder
	quote := byte(0)
	for index := 0; index < len(text); {
		character := text[index]
		if character == '\\' && quote != '\'' {
			out.WriteByte(character)
			if index+1 < len(text) {
				out.WriteByte(text[index+1])
				index += 2
				continue
			}
			index++
			continue
		}
		if quote == '\'' {
			out.WriteByte(character)
			if character == '\'' {
				quote = 0
			}
			index++
			continue
		}
		if character == '\'' && quote == 0 {
			quote = '\''
			out.WriteByte(character)
			index++
			continue
		}
		if character == '"' {
			if quote == '"' {
				quote = 0
			} else if quote == 0 {
				quote = '"'
			}
			out.WriteByte(character)
			index++
			continue
		}
		if character == '`' {
			end, err := compactKbuildBacktickCommandEnd(text, index+1)
			if err != nil {
				return "", err
			}
			out.WriteString(placeholder)
			index = end
			continue
		}
		if character == '$' && index+1 < len(text) {
			end := 0
			var err error
			switch text[index+1] {
			case '(':
				if index+2 < len(text) && text[index+2] == '(' {
					end, err = compactKbuildArithmeticEnd(text, index+3)
				} else {
					end, err = compactKbuildCommandSubstitutionEnd(text, index+2)
				}
			case '{':
				end, err = compactKbuildBracedExpansionEnd(text, index+2)
			}
			if err != nil {
				return "", err
			}
			if end != 0 {
				if text[index+1] == '{' && validKbuildCommandEnvironmentName(text[index+2:end-1]) {
					// Preserve a simple ${NAME} token so command inspection can
					// recognize a dynamic interpreter followed by an immutable
					// child script. Complex parameter operators remain masked.
					out.WriteString(text[index:end])
				} else {
					out.WriteString(placeholder)
				}
				index = end
				continue
			}
		}
		out.WriteByte(character)
		index++
	}
	return out.String(), nil
}

func scanCompactKbuildShellParameters(text string, usage *compactKbuildSourceScriptEnvironmentUsage) error {
	quote := byte(0)
	for index := 0; index < len(text); {
		character := text[index]
		if character == '\\' && quote != '\'' {
			index += 2
			continue
		}
		if quote == '\'' {
			if character == '\'' {
				quote = 0
			}
			index++
			continue
		}
		if character == '\'' && quote == 0 {
			quote = '\''
			index++
			continue
		}
		if character == '"' {
			if quote == '"' {
				quote = 0
			} else if quote == 0 {
				quote = '"'
			}
			index++
			continue
		}
		if character == '`' {
			end, err := compactKbuildBacktickCommandEnd(text, index+1)
			if err != nil {
				return err
			}
			if err := scanCompactKbuildShellParameters(text[index+1:end-1], usage); err != nil {
				return err
			}
			index = end
			continue
		}
		if character == '$' {
			next, err := scanCompactKbuildDollarExpansion(text, index, usage)
			if err != nil {
				return err
			}
			if next > index {
				index = next
				continue
			}
		}
		if quote == 0 && character == '(' && index+1 < len(text) && text[index+1] == '(' {
			end, err := compactKbuildArithmeticEnd(text, index+2)
			if err != nil {
				return err
			}
			scanCompactKbuildArithmeticIdentifiers(text[index+2:end-2], usage)
			index = end
			continue
		}
		index++
	}
	if quote != 0 {
		return fmt.Errorf("unterminated shell quote")
	}
	return nil
}

func scanCompactKbuildDollarExpansion(text string, start int, usage *compactKbuildSourceScriptEnvironmentUsage) (int, error) {
	if start+1 >= len(text) {
		return start + 1, nil
	}
	next := text[start+1]
	if next == '*' || next == '@' {
		usage.argumentVectorUses++
		return start + 2, nil
	}
	if next == '0' {
		// $0 names the immutable script itself, not an element of the argv
		// forwarded through $*/$@. It therefore cannot make a compiler search
		// operand double as an independently observed data argument.
		return start + 2, nil
	}
	if next >= '1' && next <= '9' {
		usage.observePositionalArgument(int(next - '0'))
		return start + 2, nil
	}
	if strings.ContainsRune("#?$!-", rune(next)) {
		usage.positionalArgumentUses++
		usage.positionalArgumentDynamic = true
		return start + 2, nil
	}
	if isKbuildEnvironmentNameStart(next) {
		end := start + 2
		for end < len(text) && isKbuildEnvironmentNameCharacter(text[end]) {
			end++
		}
		compactKbuildSourceScriptObserveParameterValue(usage, text[start+1:end], text, end)
		return end, nil
	}
	switch next {
	case '{':
		end, err := compactKbuildBracedExpansionEnd(text, start+2)
		if err != nil {
			return 0, err
		}
		inner := text[start+2 : end-1]
		parameter := inner
		if strings.HasPrefix(parameter, "!") {
			usage.ObservesAll = true
			parameter = parameter[1:]
		}
		if strings.HasPrefix(parameter, "#") {
			parameter = parameter[1:]
		}
		nameEnd := 0
		if nameEnd < len(parameter) && isKbuildEnvironmentNameStart(parameter[nameEnd]) {
			nameEnd++
			for nameEnd < len(parameter) && isKbuildEnvironmentNameCharacter(parameter[nameEnd]) {
				nameEnd++
			}
			if nameEnd == len(parameter) {
				compactKbuildSourceScriptObserveParameterValue(usage, parameter[:nameEnd], text, end)
			} else {
				usage.observeWholeValue(parameter[:nameEnd])
			}
		} else if parameter != "" {
			positionalIndex := 0
			positionalDigits := 0
			for positionalDigits < len(parameter) && parameter[positionalDigits] >= '0' && parameter[positionalDigits] <= '9' {
				positionalIndex = positionalIndex*10 + int(parameter[positionalDigits]-'0')
				positionalDigits++
			}
			switch {
			case parameter[0] == '@' || parameter[0] == '*':
				usage.argumentVectorUses++
			case positionalDigits != 0 && positionalIndex == 0:
				// Braced $0 expansions have the same script-identity semantics as
				// their unbraced form. Parameter operators may transform that path,
				// but never select an invocation argument.
			case positionalDigits != 0:
				usage.observePositionalArgument(positionalIndex)
			case strings.ContainsRune("#?$!-", rune(parameter[0])):
				usage.positionalArgumentUses++
				usage.positionalArgumentDynamic = true
			default:
				usage.ObservesAll = true
			}
		}
		if nameEnd < len(parameter) {
			remainder := parameter[nameEnd:]
			if strings.HasPrefix(remainder, "[") {
				scanCompactKbuildArithmeticIdentifiers(remainder, usage)
			}
			if err := scanCompactKbuildShellParameters(remainder, usage); err != nil {
				return 0, err
			}
		}
		return end, nil
	case '(':
		if start+2 < len(text) && text[start+2] == '(' {
			end, err := compactKbuildArithmeticEnd(text, start+3)
			if err != nil {
				return 0, err
			}
			expression := text[start+3 : end-2]
			scanCompactKbuildArithmeticIdentifiers(expression, usage)
			if err := scanCompactKbuildShellParameters(expression, usage); err != nil {
				return 0, err
			}
			return end, nil
		}
		end, err := compactKbuildCommandSubstitutionEnd(text, start+2)
		if err != nil {
			return 0, err
		}
		if err := scanCompactKbuildShellParameters(text[start+2:end-1], usage); err != nil {
			return 0, err
		}
		return end, nil
	default:
		// A dollar followed by a byte which cannot begin a shell parameter or
		// substitution is a literal dollar. Consume only the dollar so the outer
		// lexer still observes syntax carried by the following byte. In
		// particular, the trailing regex anchor in `grep "^NAME=y$"` must not
		// swallow the closing double quote.
		return start + 1, nil
	}
}

func compactKbuildSourceScriptObserveParameterValue(
	usage *compactKbuildSourceScriptEnvironmentUsage,
	name, text string,
	end int,
) {
	suffix, exact := compactKbuildSourceScriptParameterPathSuffix(text, end)
	if exact {
		usage.observeValuePath(name, suffix)
		return
	}
	usage.observeWholeValue(name)
}

func compactKbuildSourceScriptParameterPathSuffix(text string, end int) (string, bool) {
	if end >= len(text) || text[end] != '/' {
		return "", false
	}
	start := end + 1
	finish := start
	for finish < len(text) {
		character := text[finish]
		if character == '$' || character == '`' {
			return "", false
		}
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("/._+-@", rune(character)) {
			finish++
			continue
		}
		break
	}
	pathname := strings.TrimSuffix(text[start:finish], "/")
	canonical := canonicalKbuildRulePath(pathname)
	if canonical == "" || canonical == "." || canonical != pathname || strings.ContainsAny(pathname, "$%") {
		return "", false
	}
	return canonical, true
}

func compactKbuildBracedExpansionEnd(text string, start int) (int, error) {
	depth := 1
	quote := byte(0)
	for index := start; index < len(text); index++ {
		character := text[index]
		if character == '\\' && quote != '\'' {
			index++
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == '$' && index+1 < len(text) && text[index+1] == '{' {
			depth++
			index++
			continue
		}
		if character == '}' {
			depth--
			if depth == 0 {
				return index + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("unterminated shell parameter expansion")
}

func compactKbuildCommandSubstitutionEnd(text string, start int) (int, error) {
	depth := 1
	quote := byte(0)
	for index := start; index < len(text); index++ {
		character := text[index]
		if character == '\\' && quote != '\'' {
			index++
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == '(' {
			depth++
		} else if character == ')' {
			depth--
			if depth == 0 {
				return index + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("unterminated shell command substitution")
}

func compactKbuildArithmeticEnd(text string, start int) (int, error) {
	depth := 1
	for index := start; index+1 < len(text); index++ {
		if text[index] == '\\' {
			index++
			continue
		}
		if text[index] == '(' {
			depth++
			continue
		}
		if text[index] == ')' {
			if depth > 1 {
				depth--
				continue
			}
			if text[index+1] == ')' {
				return index + 2, nil
			}
		}
	}
	return 0, fmt.Errorf("unterminated shell arithmetic expansion")
}

func scanCompactKbuildArithmeticIdentifiers(expression string, usage *compactKbuildSourceScriptEnvironmentUsage) {
	for index := 0; index < len(expression); {
		if !isKbuildEnvironmentNameStart(expression[index]) || index > 0 && isKbuildEnvironmentNameCharacter(expression[index-1]) {
			index++
			continue
		}
		end := index + 1
		for end < len(expression) && isKbuildEnvironmentNameCharacter(expression[end]) {
			end++
		}
		usage.addArithmetic(expression[index:end])
		index = end
	}
}

func isKbuildEnvironmentNameStart(character byte) bool {
	return character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func isKbuildEnvironmentNameCharacter(character byte) bool {
	return isKbuildEnvironmentNameStart(character) || character >= '0' && character <= '9'
}

func inspectCompactKbuildSourceScriptCommands(commandText string, scan *compactKbuildSourceScriptScan) {
	tokens, err := lexCompactKbuildRecipe(commandText)
	if err != nil {
		scan.usage.ObservesAll = true
		return
	}
	words := []string{}
	locals := map[string]string{}
	skipRedirectionOperand := false
	finish := func() {
		inspectCompactKbuildSourceScriptCommand(words, scan, locals)
		words = nil
	}
	for _, token := range tokens {
		if token.operator && strings.ContainsRune(";|&()", rune(token.value[0])) {
			finish()
			skipRedirectionOperand = false
			continue
		}
		if token.operator && (token.value == "<" || token.value == ">" || token.value == ">>") {
			if len(words) != 0 && compactKbuildDecimalWord(words[len(words)-1]) {
				words = words[:len(words)-1]
			}
			skipRedirectionOperand = true
			continue
		}
		if !token.operator {
			if skipRedirectionOperand {
				skipRedirectionOperand = false
				continue
			}
			words = append(words, token.value)
		}
	}
	finish()
	// Shell arithmetic reads lexical identifiers as variables. BusyBox ash can
	// additionally treat a local variable's literal value as another arithmetic
	// expression (x=CC; $((x))). Resolve that bounded case. Runtime-computed
	// arithmetic indirection cannot acquire an omitted tool proxy: only the
	// lexical capability names collected here are placed in the action.
	resolvedArithmetic := map[string]bool{}
	var resolveArithmetic func(string)
	resolveArithmetic = func(name string) {
		if resolvedArithmetic[name] || !scan.usage.arithmeticNames[name] {
			return
		}
		value, local := locals[name]
		if !local {
			return
		}
		resolvedArithmetic[name] = true
		indirect := compactKbuildSourceScriptEnvironmentUsage{Names: map[string]bool{}}
		scanCompactKbuildArithmeticIdentifiers(value, &indirect)
		for nested := range indirect.Names {
			if _, nestedLocal := locals[nested]; nestedLocal {
				scan.usage.arithmeticNames[nested] = true
				resolveArithmetic(nested)
				continue
			}
			scan.usage.observeWholeValue(nested)
		}
	}
	for name := range scan.usage.arithmeticNames {
		resolveArithmetic(name)
	}
	clean := strings.ReplaceAll(commandText, compactKbuildLiteralDollarToken, "")
	if strings.Contains(clean, "/proc/self/environ") || strings.Contains(clean, "/proc/1/environ") || compactKbuildContainsEnvironmentToken(clean, "ENVIRON") || strings.Contains(clean, "getenv(") {
		scan.usage.ObservesAll = true
	}
}

func compactKbuildContainsEnvironmentToken(text, token string) bool {
	for offset := 0; ; {
		relative := strings.Index(text[offset:], token)
		if relative < 0 {
			return false
		}
		start := offset + relative
		end := start + len(token)
		before := start > 0 && isKbuildEnvironmentNameCharacter(text[start-1])
		after := end < len(text) && isKbuildEnvironmentNameCharacter(text[end])
		if !before && !after {
			return true
		}
		offset = end
	}
}

func compactKbuildDecimalWord(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func inspectCompactKbuildSourceScriptCommand(
	words []string,
	scan *compactKbuildSourceScriptScan,
	locals map[string]string,
) {
	inspectCompactKbuildSourceScriptCommandDepth(words, scan, locals, 0)
}

func inspectCompactKbuildSourceScriptCommandDepth(
	words []string,
	scan *compactKbuildSourceScriptScan,
	locals map[string]string,
	depth int,
) {
	if depth > maxCompactKbuildSourceScriptWrapperDepth {
		scan.usage.ObservesAll = true
		return
	}
	assignments := map[string]string{}
	for len(words) != 0 {
		word := strings.TrimLeft(words[0], "+@-")
		if name, value, assignment := compactKbuildShellAssignmentValue(word); assignment {
			assignments[name] = value
			words = words[1:]
			continue
		}
		if word == "" || word == "!" || word == "if" || word == "then" || word == "elif" || word == "else" || word == "while" || word == "until" || word == "do" || word == "time" || word == "command" || word == "builtin" || word == "exec" {
			words = words[1:]
			continue
		}
		break
	}
	if len(words) == 0 {
		for name, value := range assignments {
			value = strings.ReplaceAll(value, compactKbuildLiteralDollarToken, "$")
			if strings.Contains(value, "`") || strings.Contains(value, "$") && !compactKbuildBoundedEvalTemplate(value) {
				delete(locals, name)
				continue
			}
			locals[name] = value
		}
		return
	}
	program := strings.TrimLeft(words[0], "+@-")
	arguments := words[1:]
	positionalProgram := strings.Trim(strings.ReplaceAll(program, compactKbuildLiteralDollarToken, "$"), "'\"")
	if positionalProgram == "$*" || positionalProgram == "$@" {
		scan.usage.argumentVectorProgramUses++
	}
	base := path.Base(strings.ReplaceAll(program, compactKbuildLiteralDollarToken, "$"))
	dynamicProgram := strings.Contains(program, "$")
	if !dynamicProgram {
		scan.usage.addProgram(strings.ReplaceAll(program, compactKbuildLiteralDollarToken, "$"))
	}
	if child, exact := compactKbuildSourceScriptChildPath(program); exact {
		scan.programSources = append(scan.programSources, child)
	} else if compactKbuildDynamicSourceScriptPath(program) && !compactKbuildObjectTreeProgramPath(program) {
		scan.usage.ObservesAll = true
	}
	if compactKbuildShellProgram(base) {
		if compactKbuildShellCommandMode(arguments) {
			scan.usage.ObservesAll = true
		}
	}
	// Shells are also commonly reached through env or a multicall binary. Scan
	// every literal nested shell word so `env sh -ec ...`, `busybox sh -c ...`,
	// and split option words such as `sh -e -c ...` retain the same fail-closed
	// boundary as a direct shell invocation.
	for index, argument := range arguments {
		argument = strings.ReplaceAll(argument, compactKbuildLiteralDollarToken, "$")
		if !compactKbuildShellProgram(path.Base(argument)) {
			continue
		}
		if compactKbuildShellCommandMode(arguments[index+1:]) {
			scan.usage.ObservesAll = true
		}
	}
	if base == "sh" || base == "bash" || base == "dash" || base == "ash" || base == "ksh" || dynamicProgram {
		for _, argument := range arguments {
			if strings.HasPrefix(argument, "-") {
				continue
			}
			if child, exact := compactKbuildSourceScriptChildPath(argument); exact {
				scan.sources = append(scan.sources, child)
			} else if compactKbuildDynamicSourceScriptPath(argument) {
				scan.usage.ObservesAll = true
			}
			break
		}
	}
	switch base {
	case "eval":
		resolved := make([]string, 0, len(arguments))
		bounded := len(arguments) != 0
		for _, argument := range arguments {
			if name, exact := compactKbuildExactShellParameter(argument); exact {
				value, known := locals[name]
				if !known {
					bounded = false
					break
				}
				resolved = append(resolved, value)
				continue
			}
			if strings.ContainsAny(argument, "$`") {
				bounded = false
				break
			}
			resolved = append(resolved, strings.ReplaceAll(argument, compactKbuildLiteralDollarToken, "$"))
		}
		if !bounded {
			scan.usage.ObservesAll = true
			break
		}
		nested, err := scanCompactKbuildSourceScript(strings.Join(resolved, " "))
		if err != nil {
			scan.usage.ObservesAll = true
			break
		}
		scan.usage.merge(nested.usage)
		scan.sources = append(scan.sources, nested.sources...)
		scan.programSources = append(scan.programSources, nested.programSources...)
	case ".", "source":
		if len(arguments) == 0 || strings.Contains(arguments[0], "$") {
			scan.usage.ObservesAll = true
		} else {
			scan.sources = append(scan.sources, arguments[0])
		}
	case "printenv":
		named := false
		for _, argument := range arguments {
			if strings.HasPrefix(argument, "-") {
				continue
			}
			named = true
			argument = strings.ReplaceAll(argument, compactKbuildLiteralDollarToken, "$")
			if !validKbuildCommandEnvironmentName(argument) || strings.Contains(argument, "$") {
				scan.usage.ObservesAll = true
			} else {
				scan.usage.observeWholeValue(argument)
			}
		}
		if !named {
			scan.usage.ObservesAll = true
		}
	case "env":
		programIndex, hasProgram, exact := compactKbuildEnvProgram(arguments)
		if !exact || !hasProgram {
			scan.usage.ObservesAll = true
			break
		}
		inspectCompactKbuildSourceScriptCommandDepth(arguments[programIndex:], scan, locals, depth+1)
	case "busybox":
		if len(arguments) == 0 {
			break
		}
		applet := strings.ReplaceAll(arguments[0], compactKbuildLiteralDollarToken, "$")
		// BusyBox has a broad applet and global-option grammar. Re-enter only for
		// the exact applets whose environment/source semantics this scanner owns;
		// every other invocation remains outside this bounded projection.
		if applet == "printenv" || compactKbuildShellProgram(applet) {
			inspectCompactKbuildSourceScriptCommandDepth(arguments, scan, locals, depth+1)
		}
	case "set":
		if len(arguments) == 0 {
			scan.usage.ObservesAll = true
		}
	case "export", "readonly":
		if len(arguments) == 0 || slices.Contains(arguments, "-p") {
			scan.usage.ObservesAll = true
		}
	case "declare", "typeset":
		if slices.Contains(arguments, "-p") || slices.Contains(arguments, "-x") && len(arguments) == 1 {
			scan.usage.ObservesAll = true
		}
	case "compgen":
		if slices.Contains(arguments, "-v") || slices.Contains(arguments, "-e") {
			scan.usage.ObservesAll = true
		}
	case "test", "[":
		for index, argument := range arguments {
			if argument == "-v" && index+1 < len(arguments) {
				name := strings.ReplaceAll(arguments[index+1], compactKbuildLiteralDollarToken, "$")
				if validKbuildCommandEnvironmentName(name) {
					scan.usage.add(name)
				} else {
					scan.usage.ObservesAll = true
				}
			}
		}
	case "unset":
		for _, name := range arguments {
			delete(locals, name)
		}
	}
}

func compactKbuildShellProgram(base string) bool {
	switch base {
	case "sh", "bash", "dash", "ash", "ksh":
		return true
	default:
		return false
	}
}

// compactKbuildShellCommandMode recognizes both a standalone -c and combined
// short-option words such as -ec. It intentionally keeps scanning later option
// words: shells accept `sh -e -c command`, while a false positive after a
// source-file operand merely retains more role-bearing exports.
func compactKbuildShellCommandMode(arguments []string) bool {
	for _, argument := range arguments {
		argument = strings.ReplaceAll(argument, compactKbuildLiteralDollarToken, "$")
		if argument == "--command" || strings.HasPrefix(argument, "--command=") {
			return true
		}
		if len(argument) > 1 && argument[0] == '-' && argument[1] != '-' && strings.ContainsRune(argument[1:], 'c') {
			return true
		}
	}
	return false
}

// compactKbuildEnvProgram returns the first command operand after env options
// and NAME=VALUE assignments. Options with operands must be consumed exactly:
// otherwise `env -u NAME` is itself an environment listing and NAME must not be
// mistaken for a child program. Unknown option grammar is fail-closed.
func compactKbuildEnvProgram(arguments []string) (int, bool, bool) {
	options := true
	for index := 0; index < len(arguments); {
		argument := strings.ReplaceAll(arguments[index], compactKbuildLiteralDollarToken, "$")
		if options && argument == "--" {
			options = false
			index++
			continue
		}
		if options && strings.HasPrefix(argument, "--") {
			switch {
			case argument == "--ignore-environment", argument == "--null", argument == "--debug":
				index++
				continue
			case strings.HasPrefix(argument, "--unset="), strings.HasPrefix(argument, "--chdir="),
				strings.HasPrefix(argument, "--split-string="), strings.HasPrefix(argument, "--argv0="):
				index++
				continue
			case argument == "--unset", argument == "--chdir", argument == "--split-string", argument == "--argv0":
				if index+1 >= len(arguments) {
					return 0, false, true
				}
				index += 2
				continue
			default:
				return 0, false, false
			}
		}
		if options && len(argument) > 1 && argument[0] == '-' {
			if argument == "-" {
				index++
				continue
			}
			consumeNext := false
			for optionIndex := 1; optionIndex < len(argument); optionIndex++ {
				switch argument[optionIndex] {
				case 'i', '0', 'v':
					continue
				case 'u', 'C', 'S', 'a':
					consumeNext = optionIndex+1 == len(argument)
					optionIndex = len(argument)
				default:
					return 0, false, false
				}
			}
			if consumeNext {
				if index+1 >= len(arguments) {
					return 0, false, true
				}
				index++
			}
			index++
			continue
		}
		if compactKbuildShellAssignment(argument) {
			index++
			continue
		}
		return index, true, true
	}
	return 0, false, true
}

func compactKbuildShellAssignment(word string) bool {
	_, _, ok := compactKbuildShellAssignmentValue(word)
	return ok
}

func compactKbuildShellAssignmentValue(word string) (string, string, bool) {
	name, value, ok := strings.Cut(word, "=")
	return name, value, ok && validKbuildCommandEnvironmentName(name)
}

func compactKbuildExactShellParameter(word string) (string, bool) {
	if strings.HasPrefix(word, "$") && len(word) > 1 && isKbuildEnvironmentNameStart(word[1]) {
		name := word[1:]
		return name, validKbuildCommandEnvironmentName(name)
	}
	if strings.HasPrefix(word, "${") && strings.HasSuffix(word, "}") {
		name := word[2 : len(word)-1]
		return name, validKbuildCommandEnvironmentName(name)
	}
	return "", false
}

// compactKbuildBoundedEvalTemplate accepts a local command template only when
// its executable word is literal and safe. Later parameters remain visible to
// the recursive scanner; they may affect argv but cannot replace the selected
// program with an unprojected environment capability.
func compactKbuildBoundedEvalTemplate(value string) bool {
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return false
	}
	programSeen := false
	for _, token := range tokens {
		if token.operator {
			if token.value == "<" || token.value == ">" || token.value == ">>" {
				continue
			}
			return false
		}
		if programSeen {
			continue
		}
		programSeen = true
		program := strings.TrimLeft(strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$"), "+@-")
		if strings.ContainsAny(program, "$`") || !safeLinuxSourceScriptCommandName(program) {
			return false
		}
	}
	return programSeen
}

func compactKbuildSourceScriptChildPath(word string) (string, bool) {
	word = strings.ReplaceAll(word, compactKbuildLiteralDollarToken, "$")
	rooted := false
	prefixes := []string{
		"$srctree/", "${srctree}/", "$abs_srctree/", "${abs_srctree}/",
		"__LINUX_BZL_SOURCE_TREE__/", "${tree:kernel}/",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(word, prefix) {
			word = strings.TrimPrefix(word, prefix)
			rooted = true
			break
		}
	}
	if strings.ContainsAny(word, "$`") || path.IsAbs(word) {
		return "", false
	}
	// A suffix remains useful static path syntax, but is not required. A
	// source-rooted or slash-bearing extensionless word is equally concrete;
	// the caller verifies its immutable contents before treating a direct
	// program as shell text.
	if !rooted && !strings.Contains(word, "/") && !strings.HasSuffix(word, ".sh") {
		return "", false
	}
	word = canonicalKbuildRulePath(word)
	return word, word != ""
}

func compactKbuildDynamicSourceScriptPath(word string) bool {
	word = strings.ReplaceAll(word, compactKbuildLiteralDollarToken, "$")
	return strings.ContainsAny(word, "$`") && (strings.Contains(word, "/") || strings.Contains(word, ".sh"))
}

// compactKbuildObjectTreeProgramPath recognizes a statically named generated
// executable beneath the invocation's declared object root. Such a program is
// an action input, not dynamically sourced shell text: its pathname observes
// the named root, but does not imply that the surrounding script enumerates
// every environment capability. A dynamic suffix still fails closed.
func compactKbuildObjectTreeProgramPath(word string) bool {
	word = strings.ReplaceAll(word, compactKbuildLiteralDollarToken, "$")
	for _, prefix := range []string{
		"$objtree/", "${objtree}/", "$abs_output/", "${abs_output}/",
		"__LINUX_BZL_OBJECT_TREE__/", "${tree:prep}/", "${tree:host}/",
		"${tree:bootstrap}/", "${tree:prehost}/", "${work:root}/",
	} {
		suffix, ok := strings.CutPrefix(word, prefix)
		if !ok || suffix == "" || strings.ContainsAny(suffix, "$`") {
			continue
		}
		canonical := canonicalKbuildRulePath(suffix)
		return canonical != "" && canonical != "." && canonical == suffix
	}
	return false
}
