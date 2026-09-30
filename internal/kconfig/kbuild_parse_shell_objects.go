package kconfig

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// A source Makefile can change an object file while it is being parsed. Keep
// that Make-visible effect in a persistent view: target evaluators captured
// before a later source line must continue to observe the earlier version.
// These entries do not claim an action output or write on the analysis host.
type kbuildParseObjectView struct {
	base      KbuildVirtualFileView
	path      string
	alias     string
	content   string
	exists    bool
	exact     bool
	directory bool
	uncertain bool
	// An unresolved Make pathname can name any object-tree entry. Until its
	// producer is measured, no subsequent object read may infer absence.
	uncertainPath bool
}

// An exact object read or write failure is a selected source semantic error,
// never evidence that an ordinary Make variable is merely unresolved.
type kbuildParseObjectEffectError struct{ description string }

func (e *kbuildParseObjectEffectError) Error() string { return e.description }

// A mkdir -p operand creates every directory it traverses, including the
// directory immediately before a lexical `..`. Cleaning its final pathname
// first loses an effect visible to subsequent Make wildcards. Keep the
// component walk beneath the declared object root at every step.
func (p *kbuildParser) parseObjectMkdirDirectories(word, objectRoot string, relativeDirectory bool) ([]string, error) {
	const root = "__LINUX_BZL_OBJECT_TREE__/"
	if strings.ContainsAny(word, "'$`*?[]{}~;|&<>()\\\r\n") {
		return nil, fmt.Errorf("object-tree operand has active shell syntax")
	}
	var components []string
	switch {
	case strings.HasPrefix(word, root):
		word = strings.TrimPrefix(word, root)
	case relativeDirectory && !path.IsAbs(word):
		rel, err := filepath.Rel(objectRoot, p.workingDir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("relative directory escapes the declared object tree")
		}
		if rel != "." {
			components = strings.Split(filepath.ToSlash(rel), "/")
		}
	default:
		return nil, fmt.Errorf("operand is outside the declared object-tree root")
	}
	var directories []string
	seen := map[string]bool{}
	appendDirectory := func() error {
		if len(components) == 0 {
			return nil // The declared object root already exists.
		}
		name := strings.Join(components, "/")
		if err := validatePlanRelativePath("parse-time object-tree", name); err != nil {
			return err
		}
		if !seen[name] {
			seen[name] = true
			directories = append(directories, root+name)
		}
		return nil
	}
	for _, component := range strings.Split(word, "/") {
		switch component {
		case "", ".":
			continue
		case "..":
			if len(components) == 0 {
				return nil, fmt.Errorf("relative directory escapes the declared object tree")
			}
			if err := p.validateObjectMkdirTraversal(root+strings.Join(components, "/"), objectRoot); err != nil {
				return nil, err
			}
			components = components[:len(components)-1]
		default:
			components = append(components, component)
			if err := appendDirectory(); err != nil {
				return nil, err
			}
		}
	}
	if err := appendDirectory(); err != nil {
		return nil, err
	}
	return directories, nil
}

// A virtual file is not evidence of a directory. Only a prior parse-local
// mkdir directory with no conflicting base artifact may be traversed through
// `..`; existing physical descendants must also be actual directories, never
// symlinks. The declared object root itself is the trusted capability.
func (p *kbuildParser) validateObjectMkdirTraversal(directory, objectRoot string) error {
	const root = "__LINUX_BZL_OBJECT_TREE__/"
	if !strings.HasPrefix(directory, root) {
		return fmt.Errorf("relative directory escapes the declared object tree")
	}
	name := strings.TrimPrefix(directory, root)
	parts := strings.Split(name, "/")
	var view KbuildVirtualFileView = p.virtualFileView
	for ordinal := range parts {
		prefix := root + strings.Join(parts[:ordinal+1], "/")
		physical := filepath.Join(objectRoot, filepath.FromSlash(strings.TrimPrefix(prefix, root)))
		info, err := os.Lstat(physical)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect object-tree directory %q: %w", prefix, err)
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("object-tree directory %q traverses a non-directory or symlink", prefix)
		}
		view = p.virtualFileView
		for overlay, ok := view.(*kbuildParseObjectView); ok; overlay, ok = view.(*kbuildParseObjectView) {
			if overlay.path == prefix && (!overlay.exists || !overlay.directory || overlay.uncertain) {
				return fmt.Errorf("object-tree directory %q has an unproven prior effect", prefix)
			}
			view = overlay.base
		}
		_, exists, _, err := view.Read(prefix)
		if err != nil {
			return fmt.Errorf("inspect object-tree directory %q: %w", prefix, err)
		}
		if exists {
			return fmt.Errorf("object-tree directory %q has an untyped prior artifact", prefix)
		}
	}
	return nil
}

func (v *kbuildParseObjectView) Read(query string) (string, bool, bool, error) {
	if v.uncertainPath && !strings.HasPrefix(query, "__LINUX_BZL_SOURCE_TREE__/") {
		return "", false, false, fmt.Errorf("object-tree read %q follows an unmeasured parse-time pathname", query)
	}
	if query == v.path || (v.alias != "" && query == v.alias) {
		if v.uncertain {
			return "", false, false, fmt.Errorf("object-tree read %q follows an unmeasured conditional write", query)
		}
		return v.content, v.exists, v.exact, nil
	}
	return v.base.Read(query)
}

func (v *kbuildParseObjectView) Match(pattern string) []string {
	return v.matchWithBase(pattern, v.base.Match(pattern))
}

func (v *kbuildParseObjectView) MatchRead(pattern string) ([]string, error) {
	if v.uncertainPath && !strings.HasPrefix(pattern, "__LINUX_BZL_SOURCE_TREE__/") {
		return nil, fmt.Errorf("wildcard %q follows an unmeasured parse-time pathname", pattern)
	}
	if v.uncertain {
		query := strings.TrimSuffix(pattern, "/")
		matched, matchErr := path.Match(query, v.path)
		aliasMatched := false
		if v.alias != "" {
			aliasMatched, matchErr = path.Match(query, v.alias)
		}
		if matchErr != nil || matched || aliasMatched || query == v.path || (v.alias != "" && query == v.alias) {
			return nil, fmt.Errorf("wildcard %q observes conditional object-tree write %q before its graph guard is measured", pattern, v.path)
		}
	}
	if base, ok := v.base.(interface {
		MatchRead(string) ([]string, error)
	}); ok {
		matches, err := base.MatchRead(pattern)
		if err != nil {
			return nil, err
		}
		return v.matchWithBase(pattern, matches), nil
	}
	return v.Match(pattern), nil
}

func (v *kbuildParseObjectView) matchWithBase(pattern string, base []string) []string {
	matches := slices.DeleteFunc(slices.Clone(base), func(name string) bool {
		return name == v.path || (v.alias != "" && name == v.alias) ||
			(v.directory && (name == v.path+"/" || (v.alias != "" && name == v.alias+"/")))
	})
	matched, err := path.Match(pattern, v.path)
	if err == nil && matched && v.exists {
		matches = append(matches, v.path)
	} else if v.directory && v.exists && pattern == v.path+"/" {
		matches = append(matches, pattern)
	}
	if v.alias != "" && v.exists {
		aliasMatched, aliasErr := path.Match(pattern, v.alias)
		if aliasErr == nil && aliasMatched {
			matches = append(matches, v.alias)
		} else if v.directory && pattern == v.alias+"/" {
			matches = append(matches, pattern)
		}
	}
	slices.Sort(matches)
	return slices.Compact(matches)
}

// The source feature-status printf is a pure shell builtin: GNU Make reads
// only its bytes for an informational $(info ...) call. This exact argument
// grammar keeps diagnostics from becoming an undeclared host-shell probe.
func (p *kbuildParser) parseFeatureDiagnosticPrintf(command string) (string, bool, error) {
	if !strings.HasPrefix(command, "printf ") {
		return "", false, nil
	}
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil || len(tokens) < 3 || tokens[0].value != "printf" || tokens[1].operator {
		return "", false, nil
	}
	format := tokens[1].value
	status := format == `...%30s: [ \033[32mon\033[m  ]` || format == `...%30s: [ \033[31mOFF\033[m ]`
	plain := format == "...%30s: %s"
	if (!status || len(tokens) != 3) && (!plain || len(tokens) != 4) {
		return "", false, nil
	}
	if shell, defined := p.lookupRawVar("SHELL"); defined && shell != "/bin/sh" {
		return "", true, fmt.Errorf("%s: diagnostic printf has unsupported selected SHELL override", p.currentPos)
	}
	for _, name := range []string{"ENV", "BASH_ENV"} {
		if value, defined := p.lookupRawVar(name); defined && value != "" {
			return "", true, fmt.Errorf("%s: diagnostic printf has unsupported selected %s startup override", p.currentPos, name)
		}
	}
	arguments := make([]any, 0, len(tokens)-2)
	for _, token := range tokens[2:] {
		if token.operator || token.pathnameExpansion || token.shellExpansion || strings.Contains(token.value, compactKbuildLiteralDollarToken) {
			return "", true, fmt.Errorf("%s: diagnostic printf has an unproven shell argument", p.currentPos)
		}
		if err := ValidateKbuildOrdinaryValue("diagnostic printf argument", token.value); err != nil {
			return "", true, err
		}
		arguments = append(arguments, token.value)
	}
	result, resultErr := normalizeKbuildShellOutput(fmt.Sprintf(strings.ReplaceAll(format, `\033`, "\x1b"), arguments...))
	return result, true, resultErr
}

// parseObjectTreeShell admits only the literal source shapes used for
// parse-time object metadata. Every path must be rooted in the declared
// object namespace, and active shell syntax is rejected before changing the
// view. A source-selected child needing a physical file still needs its own
// action producer; this view represents Make's parse-time reads alone.
func (p *kbuildParser) parseObjectTreeShell(command string) (string, bool, error) {
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		return "", false, nil
	}
	if len(tokens) == 0 || tokens[0].operator {
		return "", false, nil
	}
	verb := tokens[0].value
	if verb != "mkdir" && verb != "touch" && verb != "rm" && verb != "echo" {
		return "", false, nil
	}
	if verb == "echo" {
		// An object-tree path in a compiler include argument is an input, not
		// an echo output. Only a redirect into that tree claims this writer;
		// unsupported echo pipelines still pass to the hermetic shell query.
		writesObject := false
		for index, token := range tokens[:len(tokens)-1] {
			if token.operator && (token.value == ">" || token.value == ">>") &&
				strings.Contains(tokens[index+1].value, "__LINUX_BZL_OBJECT_TREE__") {
				writesObject = true
				break
			}
		}
		if !writesObject {
			return "", false, nil
		}
	}
	// The source's mkdir -p of object subdirectories may use relative words
	// while Make is parsing from the declared object working directory.
	// All other shell probes need an explicit object-tree operand.
	objectRoot := p.sourceRoots["__LINUX_BZL_OBJECT_TREE__"]
	relativeDirectory := false
	if verb == "mkdir" && objectRoot != "" && p.workingDir != "" {
		rel, relErr := filepath.Rel(objectRoot, p.workingDir)
		relativeDirectory = relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if !strings.Contains(command, "__LINUX_BZL_OBJECT_TREE__") && !relativeDirectory {
		return "", false, nil
	}
	bad := func(reason string) (string, bool, error) {
		return "", true, &kbuildParseObjectEffectError{description: fmt.Sprintf("%s: parse-time object-tree shell %q: %s", p.currentPos, command, reason)}
	}
	// These source-side effects use the POSIX shell and its standard applet
	// semantics. A selected Make shell, command search path, or startup hook
	// changes that contract and must be measured by an executable probe.
	if shell, defined := p.lookupRawVar("SHELL"); defined && shell != "/bin/sh" {
		return bad("selected SHELL override has no declared object-effect semantics")
	}
	for _, name := range []string{"PATH", "ENV", "BASH_ENV"} {
		if value, defined := p.lookupRawVar(name); defined && value != "" {
			return bad("selected " + name + " override has no declared object-effect semantics")
		}
	}
	for _, token := range tokens {
		if token.pathnameExpansion || token.shellExpansion || strings.ContainsRune(token.value, '\x00') {
			return bad("dynamic shell word is not a declared object-tree effect")
		}
	}
	if p.virtualFileView == nil {
		return bad("no declared Make-visible object-tree view")
	}
	if _, ok := p.sourceRoots["__LINUX_BZL_OBJECT_TREE__"]; !ok {
		return bad("no declared object-tree root")
	}
	objectPath := func(word string) (string, error) {
		const root = "__LINUX_BZL_OBJECT_TREE__/"
		if !strings.HasPrefix(word, root) {
			return "", fmt.Errorf("operand is outside the declared object-tree root")
		}
		name := strings.TrimPrefix(word, root)
		if err := validatePlanRelativePath("parse-time object-tree", name); err != nil {
			return "", err
		}
		if strings.ContainsAny(name, "'$`*?[]{}~;|&<>()\\\r\n") {
			return "", fmt.Errorf("object-tree operand has active shell syntax")
		}
		return root + name, nil
	}
	word := func(index int, value string) bool { return tokens[index].value == value }
	objectAlias := func(filename string) string {
		if p.workingDir == "" || objectRoot == "" {
			return ""
		}
		relative := strings.TrimPrefix(filename, "__LINUX_BZL_OBJECT_TREE__/")
		alias, aliasErr := filepath.Rel(p.workingDir, filepath.Join(objectRoot, filepath.FromSlash(relative)))
		if aliasErr != nil || alias == "." || alias == ".." || strings.HasPrefix(alias, ".."+string(filepath.Separator)) {
			return ""
		}
		return filepath.ToSlash(alias)
	}
	selectedWrite := func(filename string) (bool, error) {
		selector, guarded, guardErr := p.activeSymbolicSelector()
		if guardErr != nil {
			return false, guardErr
		}
		if guarded {
			selected, selectErr := p.resolveKbuildSymbolic(selector)
			if selectErr != nil {
				return false, selectErr
			}
			if linuxProbeSymbolPattern.MatchString(selected) {
				p.deferredGraphGuards = append(p.deferredGraphGuards, selector)
				if p.rejectUnmeasuredGraphGuards {
					return false, fmt.Errorf("selected object write has an unmeasured graph guard")
				}
				// A later parse-time read must not mistake either possible
				// branch for an exact producer while the guard is unmeasured.
				p.virtualFileView = &kbuildParseObjectView{base: p.virtualFileView, path: filename, alias: objectAlias(filename), exists: true, uncertain: true}
				return false, nil
			}
			if selected == "" {
				return false, nil
			}
			if selected != "1" {
				return false, fmt.Errorf("graph guard did not resolve to a boolean")
			}
		}
		return true, nil
	}
	install := func(filename string, value *kbuildParseObjectView, output string) (string, bool, error) {
		value.base = p.virtualFileView
		value.alias = objectAlias(filename)
		p.virtualFileView = value
		return output, true, nil
	}
	var filename, result, content string
	var exists, exact bool
	switch verb {
	case "mkdir":
		if len(tokens) < 2 || !word(1, "-p") {
			return bad("unsupported mkdir command or effects")
		}
		if linuxProbeSymbolPattern.MatchString(command) {
			resolved, resolveErr := p.resolveKbuildSymbolic(command)
			if resolveErr != nil {
				return bad(fmt.Sprintf("resolve symbolic mkdir pathname: %v", resolveErr))
			}
			if linuxProbeSymbolPattern.MatchString(resolved) {
				p.deferredGraphGuards = append(p.deferredGraphGuards, command)
				if selector, guarded, guardErr := p.activeSymbolicSelector(); guardErr != nil {
					return bad(guardErr.Error())
				} else if guarded {
					p.deferredGraphGuards = append(p.deferredGraphGuards, selector)
				}
				if p.rejectUnmeasuredGraphGuards {
					return bad("mkdir pathname has an undeclared probe-dependent graph guard")
				}
				p.virtualFileView = &kbuildParseObjectView{base: p.virtualFileView, uncertainPath: true}
				return "", true, nil
			}
			// Re-lex the measured Make words: the probe may expand to zero,
			// one, or several pathname operands.
			return p.parseObjectTreeShell(resolved)
		}
		for _, token := range tokens[2:] {
			if token.operator {
				return bad("unsupported mkdir command or effects")
			}
			directories, directoryErr := p.parseObjectMkdirDirectories(token.value, objectRoot, relativeDirectory)
			if directoryErr != nil {
				return bad(directoryErr.Error())
			}
			for _, directory := range directories {
				filename = directory
				selected, guardErr := selectedWrite(filename)
				if guardErr != nil {
					return bad(guardErr.Error())
				}
				if selected {
					_, _, err = install(filename, &kbuildParseObjectView{path: filename, exists: true, directory: true}, "")
					if err != nil {
						return bad(err.Error())
					}
				}
			}
		}
		return "", true, nil
	case "touch":
		if len(tokens) != 5 || tokens[1].operator || !word(2, ";") || !word(3, "cat") || tokens[4].operator || tokens[1].value != tokens[4].value {
			return bad("unsupported touch and exact read sequence")
		}
		filename, err = objectPath(tokens[1].value)
		if err != nil {
			return bad(err.Error())
		}
		selected, guardErr := selectedWrite(filename)
		if guardErr != nil {
			return bad(guardErr.Error())
		}
		if !selected {
			return "", true, nil
		}
		content, exists, exact, err = p.virtualFileView.Read(filename)
		if err != nil || (exists && !exact) {
			return bad("touch and read requires exact prior object contents")
		}
		if !exists {
			content = ""
		}
		result, err = normalizeKbuildShellOutput(content)
		if err != nil {
			return bad(err.Error())
		}
		return install(filename, &kbuildParseObjectView{path: filename, exists: true, exact: true, content: content}, result)
	case "rm":
		if len(tokens) != 3 || !word(1, "-f") || tokens[2].operator {
			return bad("unsupported removal command")
		}
		filename, err = objectPath(tokens[2].value)
		if err != nil {
			return bad(err.Error())
		}
		selected, guardErr := selectedWrite(filename)
		if guardErr != nil {
			return bad(guardErr.Error())
		}
		if !selected {
			return "", true, nil
		}
		return install(filename, &kbuildParseObjectView{path: filename}, "")
	case "echo":
		if len(tokens) != 4 || tokens[1].operator || !word(2, ">>") || tokens[3].operator {
			return bad("unsupported append command")
		}
		filename, err = objectPath(tokens[3].value)
		if err != nil {
			return bad(err.Error())
		}
		selected, guardErr := selectedWrite(filename)
		if guardErr != nil {
			return bad(guardErr.Error())
		}
		if !selected {
			return "", true, nil
		}
		content, exists, exact, err = p.virtualFileView.Read(filename)
		if err != nil || (exists && !exact) {
			return bad("append requires exact prior object contents")
		}
		literal := tokens[1].value
		if strings.HasPrefix(literal, "-") || strings.ContainsAny(literal, "\\\r\n") || strings.Contains(literal, compactKbuildLiteralDollarToken) {
			return bad("echo argument is not a bounded literal")
		}
		return install(filename, &kbuildParseObjectView{path: filename, exists: true, exact: true, content: content + literal + "\n"}, "")
	}
	return "", false, nil
}
