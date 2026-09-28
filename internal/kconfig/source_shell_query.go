package kconfig

// This file lowers source-only $(shell ...) expressions which participate in
// parse-time Make state. Unlike recipe-side deferred content, their result may
// select target names and prerequisites, so it must be measured in the staged
// Kbuild probe DAG and replayed before graph selection. The command remains
// source-owned: scriptrun executes it with the selected multicall runtime. The
// planner admits only a small pure-filter language, proves every file operand
// is an immutable source, and renders every decoded word as a shell literal so
// globs and other spelling-level expansion cannot widen the validated input.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

const sourceShellQuerySourceTreeMarker = "__LINUX_BZL_SOURCE_TREE__"

func (s *KbuildProbeScopes) kbuildSourceShell(
	ctx context.Context,
	command string,
	workingDirectory string,
) (string, error) {
	if s == nil {
		return "", fmt.Errorf("Kbuild probe scopes are nil")
	}
	// Some source-only Kbuild evaluation paths reach this callback without the
	// ordinary target/host shell router. Preserve the configured action-role
	// token as the authority for those commands too. In particular, a POSIX
	// NAME=value prefix is not the program: Linux uses LC_ALL=C before its
	// compiler-version query. Only an exact role token at the command head is
	// routed; generic source filters still pass through the bounded grammar
	// below and cannot acquire compiler execution authority.
	program, err := sourceShellQueryProgramAfterEnvironment(command)
	if err != nil {
		return "", err
	}
	if ref, configured := parseKbuildActionRoleToken(program); configured {
		scope := ref.Scope
		if scope == KbuildActionRoleAutoScope {
			return "", fmt.Errorf("source-shell command has scope-neutral action-role token %q without a selected action scope", program)
		}
		evaluator := s.evaluators[scope]
		if evaluator == nil {
			return "", fmt.Errorf("configured source-shell action-role token references unavailable %s scope", scope)
		}
		if evaluator.tools[ref.Role] == "" {
			return "", fmt.Errorf("configured source-shell action-role token references unavailable %s role %q", scope, ref.Role)
		}
		value, routeErr := evaluator.KbuildShell(ctx, command)
		if routeErr != nil {
			return "", fmt.Errorf("evaluate %s-scoped source-shell compiler command: %w", scope, routeErr)
		}
		return value, nil
	}
	// A source-text query is execution-platform work and is independent of the
	// target compiler. Keep it in the host phase so cross compilation does not
	// make grep/sort/cut results target-toolset capabilities.
	evaluator := s.evaluators["host"]
	if evaluator == nil {
		return "", &LinuxProbeUnsupportedCommandError{Command: command}
	}
	return evaluator.sourceShellQuery(ctx, command, workingDirectory)
}

func (e *LinuxProbeEvaluator) sourceShellQuery(
	ctx context.Context,
	command string,
	workingDirectory string,
) (string, error) {
	if e == nil {
		return "", fmt.Errorf("Linux source-shell probe evaluator is nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	command = strings.TrimSpace(command)
	if command == "" || len(command) > 1<<20 || strings.ContainsAny(command, "\x00\r\n") {
		return "", fmt.Errorf("source-shell query is empty, invalid, or exceeds 1 MiB")
	}
	if e.tools[linuxProbeScriptRunner] == "" || e.tools[linuxProbeScriptRuntime] == "" {
		return "", fmt.Errorf("host source-shell query requires configured %s and %s roles", linuxProbeScriptRunner, linuxProbeScriptRuntime)
	}

	root, directory, relativeDirectory, relativeOperands, err := sourceShellQueryDirectory(e.sourceRoot, workingDirectory)
	if err != nil {
		return "", err
	}
	if root == "" {
		return "", &LinuxProbeUnsupportedCommandError{Architecture: e.architecture, Command: command}
	}
	// This shared validator admits literal dollars and operators only inside
	// single quotes. Active parameter/command expansion would obtain undeclared
	// ambient state and therefore remains outside the source-query language.
	if _, err := protectDeferredKbuildQueryDollars(command); err != nil {
		return "", fmt.Errorf("source-shell query has dynamic shell text: %w", err)
	}
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		return "", fmt.Errorf("lex source-shell query: %w", err)
	}
	if len(tokens) == 0 {
		return "", fmt.Errorf("source-shell query has no command")
	}

	expectProgram := true
	var current *sourceShellQueryCommand
	singleMakeWord := true
	finishCommand := func() error {
		if err := current.finish(); err != nil {
			return err
		}
		// The admitted -Ev mode preserves each complete nonmatching input
		// line. GNU Make subsequently joins those lines with spaces, so the
		// result is intentionally an unconstrained word list. The -oE mode
		// remains the bounded extraction grammar used by topology queries.
		if current.name == "grep" && current.grepInvert {
			singleMakeWord = false
		}
		return nil
	}
	script := make([]string, 0, len(tokens))
	sources := map[string]bool{linuxProbeRootAnchor: true}
	// The runtime is invoked explicitly as the shell interpreter, and every
	// command name inside the script must also be present in its declared
	// multicall applet set (or supplied by a configured override).
	programs := map[string]bool{"sh": true}
	for index, token := range tokens {
		if token.operator {
			// A live pipe must remain one shell action. Other shell control flow,
			// redirection, and background execution are not needed to read source
			// metadata and fail closed rather than broadening this query language.
			if token.value != "|" || expectProgram {
				return "", fmt.Errorf("source-shell query uses unsupported operator %q", token.value)
			}
			if err := finishCommand(); err != nil {
				return "", err
			}
			script = append(script, "|")
			expectProgram = true
			current = nil
			continue
		}
		value := strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$")
		raw := command[token.start:token.end]
		if strings.Contains(value, "${") {
			// Probe protocol placeholders are expanded before the selected shell
			// sees this script, so shell quoting cannot make their runtime values
			// literal. Source-owned text must never enter that protocol namespace.
			return "", fmt.Errorf("source-shell query word contains reserved protocol placeholder syntax: %q", value)
		}
		if expectProgram {
			name, environmentValue, assignment, assignmentErr := sourceShellQueryEnvironmentAssignment(raw, value)
			if assignmentErr != nil {
				return "", assignmentErr
			}
			if assignment {
				// Locale is the only environment input needed by this pure-filter
				// grammar. Keep it canonical so collation cannot become ambient and
				// variables such as PATH/TMPDIR cannot alter executable or file
				// authority. Re-quoting the decoded literal preserves its value while
				// suppressing any spelling-level shell expansion.
				if name != "LC_ALL" || environmentValue != "C" {
					return "", fmt.Errorf("source-shell query has unsupported environment assignment %q", value)
				}
				script = append(script, name+"="+sourceShellQueryScriptWord(environmentValue))
				continue
			}
			var commandErr error
			current, commandErr = newSourceShellQueryCommand(value)
			if commandErr != nil {
				return "", fmt.Errorf("source-shell query command %d: %w", index, commandErr)
			}
			programs[value] = true
			script = append(script, value)
			expectProgram = false
			continue
		}

		needsSource, argumentErr := current.argument(raw, value)
		if argumentErr != nil {
			return "", argumentErr
		}
		if !needsSource {
			script = append(script, sourceShellQueryScriptWord(value))
			continue
		}
		if !relativeOperands && !sourceShellQueryRootedOperand(value) {
			return "", fmt.Errorf("source-shell query %s has undeclared or escaping source operand %q", current.name, value)
		}
		if source, regular, sourceErr := sourceShellQueryFile(root, directory, value); sourceErr != nil {
			return "", sourceErr
		} else if regular {
			sources[source] = true
			runtimeOperand, relativeErr := sourceShellQueryRelativeOperand(source, relativeDirectory)
			if relativeErr != nil {
				return "", relativeErr
			}
			script = append(script, sourceShellQueryScriptWord(runtimeOperand))
			continue
		}
		return "", fmt.Errorf("source-shell query %s has undeclared or escaping source operand %q", current.name, value)
	}
	if expectProgram {
		return "", fmt.Errorf("source-shell query ends without a pipeline command")
	}
	if err := finishCommand(); err != nil {
		return "", err
	}
	if len(sources) == 1 {
		// Without an exact immutable source operand this is an ordinary unsupported
		// shell command, not authority to run a host utility during graph replay.
		return "", &LinuxProbeUnsupportedCommandError{Architecture: e.architecture, Command: command}
	}

	configured := make([]KbuildActionRoleRef, 0, len(e.tools))
	for role := range e.tools {
		configured = append(configured, KbuildActionRoleRef{Scope: e.scope, Role: role})
	}
	applets, err := compactKbuildScriptRuntimeApplets(configured, e.scope)
	if err != nil {
		return "", err
	}
	arguments := []string{
		"-interpreter", "${tool:" + linuxProbeScriptRuntime + "}",
		"-interpreter_arg", "sh",
		"-multicall", "${tool:" + linuxProbeScriptRuntime + "}",
		"-script_content", strings.Join(script, " "),
	}
	for _, applet := range applets {
		arguments = append(arguments, "-applet", applet.name+"=${tool:"+applet.role+"}")
	}
	programNames := make([]string, 0, len(programs))
	for program := range programs {
		programNames = append(programNames, program)
	}
	slices.Sort(programNames)
	for _, program := range programNames {
		arguments = append(arguments, "-require_applet", program)
	}
	arguments = append(arguments, "--")

	declaredSources := make([]string, 0, len(sources))
	for source := range sources {
		declaredSources = append(declaredSources, source)
	}
	slices.Sort(declaredSources)
	working := "${source_root:" + linuxProbeSourceRootName + "}"
	if relativeDirectory != "" {
		working += "/" + relativeDirectory
	}
	request := ProbeRequest{
		Schema:      LinuxProbeRequestSchema,
		Sources:     declaredSources,
		SourceRoots: []string{linuxProbeSourceRootName},
		Steps: []ProbeStep{{
			Name: "source-shell", Tool: linuxProbeScriptRunner,
			WorkingDirectory: working, Arguments: arguments,
		}},
		// Preserve stdout bytes here. evalShell applies GNU Make's trailing-
		// newline removal and embedded-newline-to-space conversion after replay.
		Outcome: ProbeOutcome{
			Kind: "text", Step: "source-shell", Stream: "stdout",
			GNUMakeShell: true, RequireSuccess: true, SingleMakeWord: singleMakeWord,
		},
	}
	if err := request.Validate(); err != nil {
		return "", fmt.Errorf("source-shell query request: %w", err)
	}
	return e.requestTextInScope("host", request)
}

// sourceShellQueryCommand is a deliberately small pure-filter grammar. The
// selected script runtime still owns and executes each applet; this code only
// bounds its I/O authority so every file operand is an explicit immutable
// source. Arbitrary applets or options cannot be made hermetic by token
// filtering because options such as grep -fFILE and sort -oFILE hide paths.
type sourceShellQueryCommand struct {
	name       string
	endOptions bool
	expect     string

	grepPattern  bool
	grepExtended bool
	grepOnly     bool
	grepInvert   bool
	sortReverse  bool
	sortVersion  bool
	headCount    bool
	cutDelimiter bool
	cutFields    bool
}

func newSourceShellQueryCommand(name string) (*sourceShellQueryCommand, error) {
	if !safeLinuxSourceScriptCommandName(name) {
		return nil, fmt.Errorf("has non-hermetic program %q", name)
	}
	switch name {
	case "grep", "sort", "head", "cut":
		return &sourceShellQueryCommand{name: name}, nil
	default:
		return nil, fmt.Errorf("program %q is outside the pure source-filter grammar", name)
	}
}

// argument returns true only when value must resolve to a declared source
// file. All other accepted arguments have command-specific scalar semantics.
func (c *sourceShellQueryCommand) argument(raw, value string) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("source-shell query has an argument without a command")
	}
	if c.expect != "" {
		expect := c.expect
		c.expect = ""
		switch expect {
		case "head-count":
			if !sourceShellBoundedDecimal(value, true) {
				return false, fmt.Errorf("source-shell query head has invalid count %q", value)
			}
			c.headCount = true
		case "cut-delimiter":
			if utf8.RuneCountInString(value) != 1 {
				return false, fmt.Errorf("source-shell query cut has invalid delimiter %q", value)
			}
			c.cutDelimiter = true
		case "cut-fields":
			if !sourceShellFieldList(value) {
				return false, fmt.Errorf("source-shell query cut has invalid field list %q", value)
			}
			c.cutFields = true
		default:
			return false, fmt.Errorf("source-shell query %s has invalid argument state %q", c.name, expect)
		}
		return false, nil
	}

	switch c.name {
	case "grep":
		if !c.grepPattern {
			if !c.endOptions && value == "--" {
				c.endOptions = true
				return false, nil
			}
			if !c.endOptions && strings.HasPrefix(value, "-") {
				duplicate := false
				if !sourceShellShortFlags(value, "oEv", func(flag byte) {
					switch flag {
					case 'o':
						duplicate = duplicate || c.grepOnly
						c.grepOnly = true
					case 'E':
						duplicate = duplicate || c.grepExtended
						c.grepExtended = true
					case 'v':
						duplicate = duplicate || c.grepInvert
						c.grepInvert = true
					}
				}) || duplicate {
					return false, fmt.Errorf("source-shell query grep has unsupported option %q", value)
				}
				return false, nil
			}
			if !sourceShellQueryQuotedLiteral(raw) {
				return false, fmt.Errorf("source-shell query grep pattern must be one quoted literal, got %q", raw)
			}
			c.grepPattern = true
			return false, nil
		}
		if !c.endOptions && strings.HasPrefix(value, "-") {
			return false, fmt.Errorf("source-shell query grep has an option after its pattern: %q", value)
		}
		return true, nil
	case "sort":
		if !sourceShellShortFlags(value, "rV", func(flag byte) {
			if flag == 'r' {
				c.sortReverse = true
			} else {
				c.sortVersion = true
			}
		}) {
			return false, fmt.Errorf("source-shell query sort has unsupported option or operand %q", value)
		}
		return false, nil
	case "head":
		if value == "-n" {
			c.expect = "head-count"
			return false, nil
		}
		if !strings.HasPrefix(value, "-n") || !sourceShellBoundedDecimal(strings.TrimPrefix(value, "-n"), true) {
			return false, fmt.Errorf("source-shell query head has unsupported option or operand %q", value)
		}
		c.headCount = true
		return false, nil
	case "cut":
		switch {
		case value == "-d":
			c.expect = "cut-delimiter"
		case strings.HasPrefix(value, "-d"):
			if utf8.RuneCountInString(strings.TrimPrefix(value, "-d")) != 1 {
				return false, fmt.Errorf("source-shell query cut has invalid delimiter option %q", value)
			}
			c.cutDelimiter = true
		case value == "-f":
			c.expect = "cut-fields"
		case strings.HasPrefix(value, "-f"):
			if !sourceShellFieldList(strings.TrimPrefix(value, "-f")) {
				return false, fmt.Errorf("source-shell query cut has invalid field option %q", value)
			}
			c.cutFields = true
		default:
			return false, fmt.Errorf("source-shell query cut has unsupported option or operand %q", value)
		}
		return false, nil
	default:
		return false, fmt.Errorf("source-shell query has unsupported program %q", c.name)
	}
}

func (c *sourceShellQueryCommand) finish() error {
	if c == nil {
		return fmt.Errorf("source-shell query has an empty pipeline segment")
	}
	if c.expect != "" {
		return fmt.Errorf("source-shell query %s omits %s", c.name, c.expect)
	}
	switch c.name {
	case "grep":
		extract := c.grepExtended && c.grepOnly && !c.grepInvert
		invertWholeLines := c.grepExtended && c.grepInvert && !c.grepOnly
		if !c.grepPattern || !extract && !invertWholeLines {
			return fmt.Errorf("source-shell query grep requires either -oE extraction or -Ev whole-line inversion and one quoted pattern")
		}
	case "sort":
		if !c.sortReverse || !c.sortVersion {
			return fmt.Errorf("source-shell query sort requires -rV")
		}
	case "head":
		if !c.headCount {
			return fmt.Errorf("source-shell query head requires one bounded -n count")
		}
	case "cut":
		if !c.cutDelimiter || !c.cutFields {
			return fmt.Errorf("source-shell query cut requires one delimiter and field list")
		}
	}
	return nil
}

func sourceShellQueryQuotedLiteral(raw string) bool {
	if len(raw) < 2 {
		return false
	}
	quote := raw[0]
	if (quote != '\'' && quote != '"') || raw[len(raw)-1] != quote {
		return false
	}
	for index := 1; index < len(raw)-1; index++ {
		character := raw[index]
		if character == quote {
			return false
		}
		if quote != '"' || character != '\\' {
			continue
		}
		index++
		if index >= len(raw)-1 || !strings.ContainsRune("$`\"\\", rune(raw[index])) {
			// The compact lexer intentionally normalizes escapes for evaluated
			// recipes. Only spellings with identical POSIX double-quote semantics
			// are exact enough for a source-owned parse-time query.
			return false
		}
	}
	return true
}

// sourceShellQueryProgramAfterEnvironment returns the first POSIX simple-
// command word after literal NAME=value assignments. It intentionally stops
// at the first operator and does not search later argv or pipeline segments
// for an action-role token: only the actual command head selects a tool scope.
func sourceShellQueryProgramAfterEnvironment(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" || len(command) > 1<<20 || strings.ContainsAny(command, "\x00\r\n") {
		return "", nil
	}
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		return "", nil
	}
	for _, token := range tokens {
		if token.operator {
			return "", nil
		}
		value := strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$")
		raw := command[token.start:token.end]
		_, _, assignment, assignmentErr := sourceShellQueryEnvironmentAssignment(raw, value)
		if assignmentErr != nil {
			return "", assignmentErr
		}
		if assignment {
			continue
		}
		return value, nil
	}
	return "", nil
}

// sourceShellQueryEnvironmentAssignment recognizes one literal POSIX
// NAME=value word. The shell may not compute its value: active parameter,
// command, arithmetic, tilde, glob, quote-concatenation, and escape forms are
// rejected. A whole quoted literal is decoded by the shared lexer and later
// rendered again with sourceShellQueryScriptWord.
func sourceShellQueryEnvironmentAssignment(raw, value string) (name, environmentValue string, assignment bool, err error) {
	equals := strings.IndexByte(raw, '=')
	if equals <= 0 {
		return "", "", false, nil
	}
	name = raw[:equals]
	if !validKbuildCommandEnvironmentName(name) {
		return "", "", false, nil
	}
	decodedName, decodedValue, found := strings.Cut(value, "=")
	if !found || decodedName != name {
		return "", "", true, fmt.Errorf("source-shell query has malformed environment assignment %q", raw)
	}
	rawValue := raw[equals+1:]
	if _, protectErr := protectDeferredKbuildQueryDollars(rawValue); protectErr != nil {
		return "", "", true, fmt.Errorf("source-shell query has dynamic environment assignment %q: %w", raw, protectErr)
	}
	if strings.ContainsRune(rawValue, '`') {
		return "", "", true, fmt.Errorf("source-shell query has dynamic environment assignment %q", raw)
	}
	if rawValue != "" {
		if rawValue[0] == '\'' || rawValue[0] == '"' {
			if !sourceShellQueryQuotedLiteral(rawValue) {
				return "", "", true, fmt.Errorf("source-shell query has unsafe environment assignment %q", raw)
			}
		} else {
			for _, character := range rawValue {
				if (character >= 'A' && character <= 'Z') ||
					(character >= 'a' && character <= 'z') ||
					(character >= '0' && character <= '9') ||
					strings.ContainsRune("_-.+,:/@%=", character) {
					continue
				}
				return "", "", true, fmt.Errorf("source-shell query has unsafe environment assignment %q", raw)
			}
		}
	}
	return name, decodedValue, true, nil
}

// sourceShellQueryScriptWord preserves exactly one decoded argv word after the
// caller has rejected the probe placeholder namespace. File operands are
// relative to the typed WorkingDirectory and need no late string expansion.
func sourceShellQueryScriptWord(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func sourceShellShortFlags(value, allowed string, accept func(byte)) bool {
	if len(value) < 2 || value[0] != '-' || value[1] == '-' {
		return false
	}
	seen := map[byte]bool{}
	for index := 1; index < len(value); index++ {
		flag := value[index]
		if !strings.ContainsRune(allowed, rune(flag)) || seen[flag] {
			return false
		}
		seen[flag] = true
		accept(flag)
	}
	return true
}

func sourceShellBoundedDecimal(value string, allowZero bool) bool {
	if value == "" || len(value) > 7 {
		return false
	}
	number := 0
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
		number = number*10 + int(character-'0')
		if number > 1_000_000 {
			return false
		}
	}
	return allowZero || number != 0
}

func sourceShellFieldList(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, field := range strings.Split(value, ",") {
		if strings.Count(field, "-") > 1 {
			return false
		}
		start, end, rangeValue := strings.Cut(field, "-")
		if !sourceShellBoundedDecimal(start, false) || (rangeValue && !sourceShellBoundedDecimal(end, false)) {
			return false
		}
	}
	return true
}

func sourceShellQueryDirectory(sourceRoot, workingDirectory string) (root, directory, relative string, relativeOperands bool, err error) {
	if strings.TrimSpace(sourceRoot) == "" {
		return "", "", "", false, nil
	}
	root, err = filepath.Abs(sourceRoot)
	if err != nil {
		return "", "", "", false, fmt.Errorf("resolve source-shell root: %w", err)
	}
	root = filepath.Clean(root)
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	directory = workingDirectory
	if strings.TrimSpace(directory) == "" {
		directory = root
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return "", "", "", false, fmt.Errorf("resolve source-shell working directory: %w", err)
	}
	directory = filepath.Clean(directory)
	if resolved, resolveErr := filepath.EvalSymlinks(directory); resolveErr == nil {
		directory = resolved
	}
	relative, err = filepath.Rel(root, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		// External Kbuild evaluates source Makefiles from its object tree. A
		// source-root-qualified operand remains immutable and safe there. Carry an
		// explicit false capability so even a relative ../ traversal which happens
		// to land below root cannot gain source authority from execroot layout.
		return root, directory, "", false, nil
	}
	if relative == "." {
		relative = ""
	} else {
		relative = filepath.ToSlash(relative)
		if validateErr := validateProbeSourcePath(relative); validateErr != nil {
			return "", "", "", false, fmt.Errorf("source-shell working directory: %w", validateErr)
		}
	}
	info, statErr := os.Stat(directory)
	if statErr != nil || !info.IsDir() {
		if statErr == nil {
			statErr = fmt.Errorf("not a directory")
		}
		return "", "", "", false, fmt.Errorf("inspect source-shell working directory %q: %w", directory, statErr)
	}
	return root, directory, relative, true, nil
}

func sourceShellQueryRootedOperand(value string) bool {
	return strings.HasPrefix(value, sourceShellQuerySourceTreeMarker+"/") || filepath.IsAbs(value)
}

func sourceShellQueryFile(root, directory, value string) (string, bool, error) {
	if value == "" || strings.ContainsRune(value, 0) {
		return "", false, nil
	}
	candidate := ""
	switch {
	case value == sourceShellQuerySourceTreeMarker:
		return "", false, nil
	case strings.HasPrefix(value, sourceShellQuerySourceTreeMarker+"/"):
		candidate = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(value, sourceShellQuerySourceTreeMarker+"/")))
	case filepath.IsAbs(value):
		candidate = filepath.Clean(value)
	default:
		candidate = filepath.Join(directory, filepath.FromSlash(value))
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", false, nil
	}
	info, err := os.Stat(candidate)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect source-shell operand %q: %w", value, err)
	}
	if !info.Mode().IsRegular() {
		return "", false, nil
	}
	resolvedRoot := root
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		resolvedRoot = resolved
	}
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", false, fmt.Errorf("resolve source-shell operand %q: %w", value, err)
	}
	resolvedRelative, err := filepath.Rel(resolvedRoot, resolvedCandidate)
	if err != nil || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) || filepath.IsAbs(resolvedRelative) {
		return "", false, fmt.Errorf("source-shell operand %q escapes the selected source root", value)
	}
	relative = filepath.ToSlash(relative)
	if err := validateProbeSourcePath(relative); err != nil {
		return "", false, err
	}
	return relative, true, nil
}

// sourceShellQueryRelativeOperand keeps runtime source paths out of the shell
// placeholder language. The request's typed WorkingDirectory already binds
// the selected source root, and a literal relative operand remains safe even
// when an execroot component contains shell punctuation.
func sourceShellQueryRelativeOperand(source, relativeDirectory string) (string, error) {
	base := filepath.FromSlash(relativeDirectory)
	if base == "" {
		base = "."
	}
	value, err := filepath.Rel(base, filepath.FromSlash(source))
	if err != nil || value == "" || value == "." || filepath.IsAbs(value) {
		if err == nil {
			err = fmt.Errorf("invalid relative source operand")
		}
		return "", fmt.Errorf("render source-shell operand %q from %q: %w", source, relativeDirectory, err)
	}
	value = filepath.ToSlash(value)
	if strings.HasPrefix(value, "-") {
		value = "./" + value
	}
	return value, nil
}
