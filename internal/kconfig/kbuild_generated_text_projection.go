package kconfig

import "strings"

const compactKbuildGeneratedTextPrepPrefix = "${tree:prep}/"

type compactKbuildGeneratedTextFile struct {
	content   string
	ambiguous bool
}

// CompactKbuildGeneratedTextProjection returns the exact bytes written to
// target by a deliberately small, data-only Kbuild recipe.
//
// The accepted shell language is either a single command redirected to target
// or Linux's conventional brace group:
//
//	{ echo words; cat object/file ...; :; } > target
//
// Commands may be echo without options, cat of files whose exact contents are
// supplied by exactFiles, printf with the sole format %s\n, or the no-op :.
// Object-file names may be plain paths or use the ${tree:prep}/ action-plan
// alias. Cat preserves operand order and duplicates. Unsupported shell syntax,
// an unknown or ambiguous file, and a write to another target all fail closed.
// An exactly empty result is reported as ("", true).
func CompactKbuildGeneratedTextProjection(recipe, target string, exactFiles map[string]string) (string, bool) {
	files := compactKbuildGeneratedTextFiles(exactFiles)
	return CompactKbuildGeneratedTextProjectionWithResolver(
		recipe,
		target,
		func(path string) (string, bool) {
			path, ok := compactKbuildGeneratedTextObjectPath(path)
			if !ok {
				return "", false
			}
			file, found := files[path]
			if !found || file.ambiguous {
				return "", false
			}
			return file.content, true
		},
	)
}

// CompactKbuildGeneratedTextProjectionWithResolver is the demand-driven form
// of CompactKbuildGeneratedTextProjection. resolveExactFile receives each path
// read by cat in operand order and returns its exact bytes. Plain operands are
// canonical paths relative to the recipe's working directory;
// ${tree:prep}/ operands retain that prefix followed by their canonical
// object-tree path. Recipes which do not read files never call the resolver.
func CompactKbuildGeneratedTextProjectionWithResolver(
	recipe, target string,
	resolveExactFile func(path string) (string, bool),
) (string, bool) {
	var ok bool
	recipe, ok = compactKbuildGeneratedTextRecipe(recipe)
	if !ok {
		return "", false
	}
	// The compact lexer treats newlines as ordinary word separators and has a
	// deliberately narrower double-quote escape model than a shell. Decline
	// those spellings rather than assigning them byte provenance accidentally.
	if strings.ContainsAny(recipe, "\r\n") {
		return "", false
	}
	target, ok = compactKbuildGeneratedTextObjectPath(target)
	if !ok {
		return "", false
	}
	tokens, err := lexCompactKbuildRecipe(recipe)
	if err != nil || len(tokens) == 0 {
		return "", false
	}
	for _, token := range tokens {
		if token.operator || token.start < 0 || token.end > len(recipe) {
			continue
		}
		spelling := recipe[token.start:token.end]
		if strings.Contains(spelling, `\`) && spelling != `'%s\n'` {
			return "", false
		}
	}
	if compactKbuildGeneratedTextBrace(recipe, tokens[0], "{") {
		if content, exact := compactKbuildGeneratedTextGroup(recipe, tokens, target, resolveExactFile); exact {
			return content, true
		}
	} else if content, exact := compactKbuildGeneratedTextDirect(recipe, tokens, target, resolveExactFile); exact {
		return content, true
	}
	return compactKbuildGeneratedTextSequence(recipe, tokens, target, resolveExactFile)
}

// compactKbuildGeneratedTextRecipe removes Make's execution-only recipe
// prefixes before interpreting the selected shell text. Kbuild's cmd wrapper
// supplies @ from inside a recursively expanded variable, so it remains in
// the selected command even though Make removes it before invoking the shell.
// '+' has the same byte-neutral execution semantics. '-' suppresses a shell
// failure and can expose partial output, so exact projection rejects it.
func compactKbuildGeneratedTextRecipe(recipe string) (string, bool) {
	recipe = strings.TrimSpace(recipe)
	for recipe != "" {
		switch recipe[0] {
		case '@', '+':
			recipe = strings.TrimSpace(recipe[1:])
		case '-':
			return "", false
		default:
			return recipe, true
		}
	}
	return "", false
}

// compactKbuildGeneratedTextSequence recognizes the shell scaffolding emitted
// by Linux's if_changed wrapper around a data-only target writer. The wrapper
// installs non-EXIT signal traps and writes a separate saved-command file; the
// one command redirected to target remains the sole source of target bytes.
// Every other command must be structurally proven incapable of writing target.
func compactKbuildGeneratedTextSequence(
	recipe string,
	tokens []compactKbuildRecipeToken,
	target string,
	resolveExactFile func(path string) (string, bool),
) (string, bool) {
	segments, ok := compactKbuildGeneratedTextCommandSegments(recipe, tokens)
	if !ok || len(segments) < 2 {
		return "", false
	}
	projected := ""
	writers := 0
	for _, segment := range segments {
		if compactKbuildGeneratedTextWrapperCommand(recipe, segment) {
			continue
		}
		redirect, content, exact, safe := compactKbuildGeneratedTextRedirectedCommand(
			recipe, segment, resolveExactFile,
		)
		if !safe {
			return "", false
		}
		if redirect != target {
			continue
		}
		if !exact || writers != 0 {
			return "", false
		}
		projected = content
		writers++
	}
	return projected, writers == 1
}

func compactKbuildGeneratedTextCommandSegments(
	recipe string,
	tokens []compactKbuildRecipeToken,
) ([][]compactKbuildRecipeToken, bool) {
	segments := [][]compactKbuildRecipeToken{}
	start := 0
	braceDepth := 0
	parenDepth := 0
	for index, token := range tokens {
		switch {
		case compactKbuildGeneratedTextBrace(recipe, token, "{"):
			braceDepth++
		case compactKbuildGeneratedTextBrace(recipe, token, "}"):
			braceDepth--
			if braceDepth < 0 {
				return nil, false
			}
		case token.operator && token.value == "(":
			parenDepth++
		case token.operator && token.value == ")":
			parenDepth--
			if parenDepth < 0 {
				return nil, false
			}
		case token.operator && token.value == ";" && braceDepth == 0 && parenDepth == 0:
			if index > start {
				segments = append(segments, tokens[start:index])
			}
			start = index + 1
		case token.operator && token.value != ">" && braceDepth == 0 && parenDepth == 0:
			return nil, false
		}
	}
	if braceDepth != 0 || parenDepth != 0 {
		return nil, false
	}
	if start < len(tokens) {
		segments = append(segments, tokens[start:])
	}
	return segments, len(segments) != 0
}

func compactKbuildGeneratedTextWrapperCommand(recipe string, tokens []compactKbuildRecipeToken) bool {
	if len(tokens) == 2 && !tokens[0].operator && !tokens[1].operator &&
		tokens[0].value == "set" && tokens[1].value == "-e" {
		return true
	}
	if len(tokens) < 3 || tokens[0].operator || tokens[0].value != "trap" {
		return false
	}
	// A signal trap cannot affect a successfully completed recipe. EXIT and the
	// shell's pseudo-signals can run on the success path and are rejected.
	for index, token := range tokens[1:] {
		if token.operator || strings.Contains(token.value, "`") || strings.Contains(token.value, "$") {
			return false
		}
		if index == 0 {
			spelling := recipe[token.start:token.end]
			if token.value != "-" && !strings.HasPrefix(spelling, "'") && !strings.HasPrefix(spelling, "\"") {
				return false
			}
			continue
		}
		signal := strings.TrimPrefix(strings.ToUpper(token.value), "SIG")
		switch signal {
		case "", "0", "EXIT", "ERR", "DEBUG", "RETURN":
			return false
		}
		for _, character := range signal {
			if (character < 'A' || character > 'Z') && (character < '0' || character > '9') {
				return false
			}
		}
	}
	return true
}

func compactKbuildGeneratedTextRedirectedCommand(
	recipe string,
	tokens []compactKbuildRecipeToken,
	resolveExactFile func(path string) (string, bool),
) (redirect, content string, exact, safe bool) {
	redirectIndex := -1
	if len(tokens) != 0 && compactKbuildGeneratedTextBrace(recipe, tokens[0], "{") {
		closing := -1
		for index := 1; index < len(tokens); index++ {
			if compactKbuildGeneratedTextBrace(recipe, tokens[index], "{") {
				return "", "", false, false
			}
			if compactKbuildGeneratedTextBrace(recipe, tokens[index], "}") {
				closing = index
				break
			}
		}
		if closing < 0 || closing+2 >= len(tokens) || !tokens[closing+1].operator || tokens[closing+1].value != ">" ||
			!compactKbuildGeneratedTextTerminal(tokens[closing+3:]) {
			return "", "", false, false
		}
		redirectIndex = closing + 1
	} else {
		for index, token := range tokens {
			if !token.operator {
				continue
			}
			if token.value != ">" || redirectIndex >= 0 {
				return "", "", false, false
			}
			redirectIndex = index
		}
		if redirectIndex <= 0 || redirectIndex+1 >= len(tokens) ||
			!compactKbuildGeneratedTextTerminal(tokens[redirectIndex+2:]) {
			return "", "", false, false
		}
		previous := tokens[redirectIndex-1]
		if previous.end == tokens[redirectIndex].start && compactKbuildGeneratedTextDigits(previous.value) {
			return "", "", false, false
		}
		program, literal := compactKbuildGeneratedTextLiteral(tokens[0])
		if !literal {
			return "", "", false, false
		}
		if _, allowed := compactKbuildGeneratedTextApplet(program); !allowed {
			return "", "", false, false
		}
		for _, token := range tokens[1:redirectIndex] {
			if token.operator || strings.Contains(token.value, "`") || strings.Contains(token.value, "$") {
				return "", "", false, false
			}
		}
	}
	redirect, ok := compactKbuildGeneratedTextTokenPath(tokens[redirectIndex+1])
	if !ok {
		return "", "", false, false
	}
	if compactKbuildGeneratedTextBrace(recipe, tokens[0], "{") {
		if _, structural := compactKbuildGeneratedTextGroup(
			recipe, tokens, redirect, func(string) (string, bool) { return "", true },
		); !structural {
			return "", "", false, false
		}
		content, exact = compactKbuildGeneratedTextGroup(recipe, tokens, redirect, resolveExactFile)
	} else {
		content, exact = compactKbuildGeneratedTextDirect(recipe, tokens, redirect, resolveExactFile)
	}
	return redirect, content, exact, true
}

// compactKbuildGeneratedTextRedirectsTarget recognizes the outer overwrite
// redirection shared by exact generated-text projections. It intentionally
// does not classify the commands inside the group: opening the redirection
// materializes the target even when their bytes are opaque, while the exact
// projection above remains fail-closed on those bytes.
func compactKbuildGeneratedTextRedirectsTarget(recipe, target string) bool {
	var recipeOK bool
	recipe, recipeOK = compactKbuildGeneratedTextRecipe(recipe)
	if !recipeOK {
		return false
	}
	target, ok := compactKbuildGeneratedTextObjectPath(target)
	if !ok {
		return false
	}
	tokens, err := lexCompactKbuildRecipe(recipe)
	if err != nil || len(tokens) == 0 {
		return false
	}
	if compactKbuildGeneratedTextBrace(recipe, tokens[0], "{") {
		closing := -1
		for index := 1; index < len(tokens); index++ {
			if compactKbuildGeneratedTextBrace(recipe, tokens[index], "{") {
				return false
			}
			if compactKbuildGeneratedTextBrace(recipe, tokens[index], "}") {
				closing = index
				break
			}
		}
		if closing < 0 || closing+2 >= len(tokens) || !tokens[closing+1].operator || tokens[closing+1].value != ">" {
			return false
		}
		redirectTarget, ok := compactKbuildGeneratedTextTokenPath(tokens[closing+2])
		return ok && redirectTarget == target && compactKbuildGeneratedTextTerminal(tokens[closing+3:])
	}
	segments, ok := compactKbuildGeneratedTextCommandSegments(recipe, tokens)
	if !ok || len(segments) < 2 {
		return false
	}
	writes := 0
	for _, segment := range segments {
		if compactKbuildGeneratedTextWrapperCommand(recipe, segment) {
			continue
		}
		redirect, _, _, safe := compactKbuildGeneratedTextRedirectedCommand(
			recipe, segment, func(string) (string, bool) { return "", true },
		)
		if !safe {
			return false
		}
		if redirect == target {
			writes++
		}
	}
	return writes == 1
}

func compactKbuildGeneratedTextGroup(
	recipe string,
	tokens []compactKbuildRecipeToken,
	target string,
	resolveExactFile func(path string) (string, bool),
) (string, bool) {
	closing := -1
	for index := 1; index < len(tokens); index++ {
		if compactKbuildGeneratedTextBrace(recipe, tokens[index], "{") {
			return "", false
		}
		if compactKbuildGeneratedTextBrace(recipe, tokens[index], "}") {
			closing = index
			break
		}
	}
	if closing < 0 || closing+2 >= len(tokens) || !tokens[closing+1].operator || tokens[closing+1].value != ">" {
		return "", false
	}
	redirectTarget, ok := compactKbuildGeneratedTextTokenPath(tokens[closing+2])
	if !ok || redirectTarget != target || !compactKbuildGeneratedTextTerminal(tokens[closing+3:]) {
		return "", false
	}

	body := tokens[1:closing]
	if len(body) == 0 {
		return "", false
	}
	var output strings.Builder
	commandStart := 0
	for index, token := range body {
		if !token.operator || token.value != ";" {
			if token.operator {
				return "", false
			}
			continue
		}
		if index == commandStart {
			return "", false
		}
		content, exact := compactKbuildGeneratedTextCommand(body[commandStart:index], target, resolveExactFile)
		if !exact {
			return "", false
		}
		output.WriteString(content)
		commandStart = index + 1
	}
	// A shell brace group requires its final command to be terminated before }.
	if commandStart != len(body) {
		return "", false
	}
	return output.String(), true
}

func compactKbuildGeneratedTextDirect(
	recipe string,
	tokens []compactKbuildRecipeToken,
	target string,
	resolveExactFile func(path string) (string, bool),
) (string, bool) {
	redirect := -1
	for index, token := range tokens {
		if !token.operator {
			continue
		}
		if token.value == ";" && index == len(tokens)-1 {
			continue
		}
		if token.value != ">" || redirect >= 0 {
			return "", false
		}
		redirect = index
	}
	if redirect <= 0 || redirect+1 >= len(tokens) {
		return "", false
	}
	// In "2>file", 2 is an IO_NUMBER rather than an argv word. The compact
	// lexer intentionally does not classify it, so reject all such adjacency.
	previous := tokens[redirect-1]
	if previous.end == tokens[redirect].start && compactKbuildGeneratedTextDigits(previous.value) {
		return "", false
	}
	redirectTarget, ok := compactKbuildGeneratedTextTokenPath(tokens[redirect+1])
	if !ok || redirectTarget != target || !compactKbuildGeneratedTextTerminal(tokens[redirect+2:]) {
		return "", false
	}
	return compactKbuildGeneratedTextCommand(tokens[:redirect], target, resolveExactFile)
}

func compactKbuildGeneratedTextCommand(
	tokens []compactKbuildRecipeToken,
	target string,
	resolveExactFile func(path string) (string, bool),
) (string, bool) {
	if len(tokens) == 0 {
		return "", false
	}
	for _, token := range tokens {
		if token.operator {
			return "", false
		}
	}
	program, ok := compactKbuildGeneratedTextLiteral(tokens[0])
	if !ok {
		return "", false
	}
	program, ok = compactKbuildGeneratedTextApplet(program)
	if !ok {
		return "", false
	}

	switch program {
	case ":":
		if len(tokens) != 1 {
			return "", false
		}
		return "", true
	case "echo":
		arguments, ok := compactKbuildGeneratedTextOutputLiterals(tokens[1:])
		if !ok || (len(arguments) != 0 && strings.HasPrefix(arguments[0], "-")) {
			return "", false
		}
		for _, argument := range arguments {
			// POSIX leaves echo's treatment of backslashes unspecified.
			if strings.Contains(argument, `\`) {
				return "", false
			}
		}
		return strings.Join(arguments, " ") + "\n", true
	case "printf":
		if len(tokens) < 2 {
			return "", false
		}
		format, ok := compactKbuildGeneratedTextLiteral(tokens[1])
		if !ok || format != `%s\n` {
			return "", false
		}
		arguments, ok := compactKbuildGeneratedTextOutputLiterals(tokens[2:])
		if !ok {
			return "", false
		}
		if len(arguments) == 0 {
			return "\n", true
		}
		return strings.Join(arguments, "\n") + "\n", true
	case "cat":
		if len(tokens) < 2 || resolveExactFile == nil {
			return "", false
		}
		var output strings.Builder
		for _, token := range tokens[1:] {
			if strings.HasPrefix(token.value, "-") {
				return "", false
			}
			name, ok := compactKbuildGeneratedTextTokenPath(token)
			if !ok || name == target {
				return "", false
			}
			resolverPath := name
			if strings.HasPrefix(token.value, compactKbuildGeneratedTextPrepPrefix) {
				resolverPath = compactKbuildGeneratedTextPrepPrefix + name
			}
			content, exact := resolveExactFile(resolverPath)
			if !exact {
				return "", false
			}
			output.WriteString(content)
		}
		return output.String(), true
	default:
		return "", false
	}
}

func compactKbuildGeneratedTextApplet(program string) (string, bool) {
	switch program {
	case ":", "echo", "printf", "cat":
		return program, true
	}
	applet, ok := compactKbuildAbsoluteRuntimeApplet(program)
	if !ok || (program != "/bin/"+applet && program != "/usr/bin/"+applet) {
		return "", false
	}
	switch applet {
	case "echo", "printf", "cat":
		return applet, true
	default:
		return "", false
	}
}

func compactKbuildGeneratedTextLiterals(tokens []compactKbuildRecipeToken) ([]string, bool) {
	values := make([]string, 0, len(tokens))
	for _, token := range tokens {
		value, ok := compactKbuildGeneratedTextLiteral(token)
		if !ok {
			return nil, false
		}
		values = append(values, value)
	}
	return values, true
}

func compactKbuildGeneratedTextOutputLiterals(tokens []compactKbuildRecipeToken) ([]string, bool) {
	values := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if !token.operator && strings.HasPrefix(token.value, compactKbuildGeneratedTextPrepPrefix) {
			value, ok := compactKbuildGeneratedTextTokenPath(token)
			if !ok {
				return nil, false
			}
			// The prep-tree marker is planner provenance for the object-tree
			// root. Kbuild's generated text records its cwd-relative graph path,
			// as the real shell command does after action path canonicalization.
			values = append(values, value)
			continue
		}
		value, ok := compactKbuildGeneratedTextLiteral(token)
		if !ok {
			return nil, false
		}
		values = append(values, value)
	}
	return values, true
}

func compactKbuildGeneratedTextLiteral(token compactKbuildRecipeToken) (string, bool) {
	if token.operator || strings.ContainsRune(token.value, 0) || strings.Contains(token.value, "$") || strings.Contains(token.value, "`") {
		return "", false
	}
	value := strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$")
	// Globbing and tilde expansion happen after lexical word splitting. Quote
	// provenance is intentionally not reconstructed here, so conservatively
	// decline them even when a particular spelling may have quoted the byte.
	if strings.ContainsAny(value, "*?[{}") || strings.HasPrefix(value, "~") || strings.HasPrefix(value, "#") {
		return "", false
	}
	return value, true
}

func compactKbuildGeneratedTextTokenPath(token compactKbuildRecipeToken) (string, bool) {
	if token.operator || strings.Contains(token.value, compactKbuildLiteralDollarToken) || strings.Contains(token.value, "`") {
		return "", false
	}
	return compactKbuildGeneratedTextObjectPath(token.value)
}

func compactKbuildGeneratedTextObjectPath(value string) (string, bool) {
	if strings.ContainsRune(value, 0) || strings.Contains(value, `\`) || strings.ContainsAny(value, "*?[`") || strings.HasPrefix(value, "~") || strings.HasPrefix(value, "#") {
		return "", false
	}
	if strings.HasPrefix(value, compactKbuildGeneratedTextPrepPrefix) {
		value = strings.TrimPrefix(value, compactKbuildGeneratedTextPrepPrefix)
	} else if strings.Contains(value, "$") {
		return "", false
	}
	if strings.ContainsAny(value, "${}") {
		return "", false
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." {
			return "", false
		}
	}
	value = canonicalKbuildRulePath(value)
	if err := validatePlanRelativePath("generated-text", value); err != nil {
		return "", false
	}
	return value, true
}

func compactKbuildGeneratedTextFiles(exactFiles map[string]string) map[string]compactKbuildGeneratedTextFile {
	files := make(map[string]compactKbuildGeneratedTextFile, len(exactFiles))
	for name, content := range exactFiles {
		name, ok := compactKbuildGeneratedTextObjectPath(name)
		if !ok {
			continue
		}
		file, found := files[name]
		if found && file.content != content {
			file.ambiguous = true
			files[name] = file
			continue
		}
		if !found {
			files[name] = compactKbuildGeneratedTextFile{content: content}
		}
	}
	return files
}

func compactKbuildGeneratedTextBrace(recipe string, token compactKbuildRecipeToken, brace string) bool {
	return !token.operator && token.value == brace && token.start >= 0 && token.end <= len(recipe) && recipe[token.start:token.end] == brace
}

func compactKbuildGeneratedTextTerminal(tokens []compactKbuildRecipeToken) bool {
	return len(tokens) == 0 || (len(tokens) == 1 && tokens[0].operator && tokens[0].value == ";")
}

func compactKbuildGeneratedTextDigits(value string) bool {
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
