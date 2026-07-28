package kconfig

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// configSourceScanner over-approximates the CONFIG_* symbols that a translation
// unit may read by scanning the source and its transitive on-disk include
// closure. It intentionally ignores preprocessor gating and counts tokens in
// comments: extra dependencies only reduce sharing, while a missed dependency
// could incorrectly share objects compiled from different effective sources.
//
// The scan is config-independent, so one scanner is shared across all configs
// in a compact metadata run and memoizes per-file work.
type configSourceScanner struct {
	sourceRoot   string
	sourceRoots  map[string]string
	includeRoots []string
	generated    map[string][]string

	fileRefs        map[string][]string
	fileIncs        map[string][]string
	fileDynamicIncs map[string][]string
	closure         map[string]sourceClosure
}

type sourceClosure struct {
	refs                   []string
	sourceIncludes         []string
	sourceIncludesComplete bool
}

type sourceForcedInclude struct {
	path   string
	direct bool
}

func newConfigSourceScanner(opts CompactMetadataOptions) *configSourceScanner {
	roots := []string{"include", "include/uapi"}
	if opts.Srcarch != "" {
		roots = append(roots,
			"arch/"+opts.Srcarch+"/include",
			"arch/"+opts.Srcarch+"/include/uapi",
		)
	}
	return &configSourceScanner{
		sourceRoot:      opts.SourceRoot,
		sourceRoots:     opts.SourceRoots,
		includeRoots:    roots,
		generated:       opts.SourceGeneratedIncludes,
		fileRefs:        map[string][]string{},
		fileIncs:        map[string][]string{},
		fileDynamicIncs: map[string][]string{},
		closure:         map[string]sourceClosure{},
	}
}

// refsForSource returns the sorted CONFIG_* symbols reachable from source.
// extraIncludeRoots contains tree-relative directories extracted from the
// object's Kbuild -I flags.
func (s *configSourceScanner) refsForSource(source string, extraIncludeRoots []string) []string {
	return append([]string(nil), s.closureForSource(source, extraIncludeRoots, nil, false).refs...)
}

func (s *configSourceScanner) closureForPreincludes(source string, extraIncludeRoots []string, sourceSearchComplete bool) sourceClosure {
	refset := map[string]bool{}
	includeSet := map[string]bool{}
	complete := true
	for _, path := range sourcePreincludePaths(source) {
		if _, ok := s.absForTreePath(path); !ok {
			continue
		}
		closure := s.closureForSource(path, extraIncludeRoots, nil, sourceSearchComplete)
		if !closure.sourceIncludesComplete {
			complete = false
		}
		for _, ref := range closure.refs {
			refset[ref] = true
		}
		for _, include := range closure.sourceIncludes {
			includeSet[include] = true
		}
	}
	refs := make([]string, 0, len(refset))
	for ref := range refset {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	includes := make([]string, 0, len(includeSet))
	for include := range includeSet {
		includes = append(includes, include)
	}
	sort.Strings(includes)
	return sourceClosure{
		refs:                   refs,
		sourceIncludes:         includes,
		sourceIncludesComplete: complete,
	}
}

// sourceIncludesForSource returns sorted source-tree paths reached through
// recursively resolved literal includes. The boolean is false when the closure
// contains an unresolved computed include that requires the blanket fallback.
func (s *configSourceScanner) sourceIncludesForSource(source string, extraIncludeRoots []string) ([]string, bool) {
	closure := s.closureForSource(source, extraIncludeRoots, nil, false)
	return append([]string(nil), closure.sourceIncludes...), closure.sourceIncludesComplete
}

// sourceSearchComplete means every source-tree include search root used by the
// compile action is represented by extraIncludeRoots. An unresolved literal is
// then generated, toolchain-provided, inactive, or a compile error rather than
// an omitted source-tree input.
func (s *configSourceScanner) closureForSource(source string, extraIncludeRoots []string, forcedIncludes []sourceForcedInclude, sourceSearchComplete bool) sourceClosure {
	if s == nil || source == "" {
		return sourceClosure{}
	}
	keyParts := append([]string{source}, extraIncludeRoots...)
	for _, include := range forcedIncludes {
		keyParts = append(keyParts, include.path, fmt.Sprintf("direct=%t", include.direct))
	}
	keyParts = append(keyParts, fmt.Sprintf("source_search_complete=%t", sourceSearchComplete))
	key := strings.Join(keyParts, "\x00")
	if cached, ok := s.closure[key]; ok {
		return cached
	}
	source, ok := cleanSourceTreePath(source)
	if !ok {
		s.closure[key] = sourceClosure{}
		return sourceClosure{}
	}
	_, ok = s.absForTreePath(source)
	if !ok {
		s.closure[key] = sourceClosure{}
		return sourceClosure{}
	}
	refset := map[string]bool{}
	includeSet := map[string]bool{}
	visited := map[string]bool{}
	sourceIncludesComplete := true
	stack := []string{source}
	for _, include := range forcedIncludes {
		resolved, satisfied := s.resolveForcedInclude(source, include, extraIncludeRoots)
		if !satisfied {
			sourceIncludesComplete = false
		}
		for _, path := range resolved {
			if path != source {
				includeSet[path] = true
			}
			stack = append(stack, path)
		}
	}
	for len(stack) > 0 {
		treePath := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[treePath] {
			continue
		}
		visited[treePath] = true
		abs, ok := s.absForTreePath(treePath)
		if !ok {
			continue
		}
		refs, rawIncludes, dynamicIncludes := s.scanFile(abs)
		if len(dynamicIncludes) != 0 {
			sourceIncludesComplete = false
		}
		for _, ref := range refs {
			refset[ref] = true
		}
		for _, inc := range rawIncludes {
			resolvedIncludes, satisfied := s.resolveLiteralInclude(treePath, inc, extraIncludeRoots)
			if !satisfied && !sourceSearchComplete {
				sourceIncludesComplete = false
			}
			for _, resolved := range resolvedIncludes {
				if resolved != source {
					includeSet[resolved] = true
				}
				if !visited[resolved] {
					stack = append(stack, resolved)
				}
			}
		}
	}
	out := make([]string, 0, len(refset))
	for ref := range refset {
		out = append(out, ref)
	}
	sort.Strings(out)
	includes := make([]string, 0, len(includeSet))
	for include := range includeSet {
		includes = append(includes, include)
	}
	sort.Strings(includes)
	result := sourceClosure{
		refs:                   out,
		sourceIncludes:         includes,
		sourceIncludesComplete: sourceIncludesComplete,
	}
	s.closure[key] = result
	return result
}

func sourcePreincludePaths(source string) []string {
	paths := []string{
		"include/linux/compiler-version.h",
		"include/linux/kconfig.h",
	}
	if filepath.Ext(source) == ".c" || strings.HasSuffix(source, ".c_shipped") {
		paths = append(paths, "include/linux/compiler_types.h")
	}
	return paths
}

func (s *configSourceScanner) resolveForcedInclude(source string, include sourceForcedInclude, extraRoots []string) ([]string, bool) {
	if include.direct {
		path, ok := cleanSourceTreePath(include.path)
		if !ok {
			return nil, false
		}
		if _, ok := s.absForTreePath(path); ok {
			return []string{path}, true
		}
		return s.resolveGeneratedInclude(path)
	}
	return s.resolveLiteralInclude(source, include.path, extraRoots)
}

func (s *configSourceScanner) resolveLiteralInclude(fromTreePath, include string, extraRoots []string) ([]string, bool) {
	resolved := s.resolveInclude(fromTreePath, include, extraRoots)
	include = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(include))))
	generated, generatedSatisfied := s.resolveGeneratedInclude(include)
	for _, path := range generated {
		resolved = appendUnique(resolved, path)
	}
	return resolved, len(resolved) != 0 || generatedSatisfied
}

func (s *configSourceScanner) resolveGeneratedInclude(include string) ([]string, bool) {
	backings, ok := s.generated[include]
	if !ok {
		return nil, false
	}
	if len(backings) == 0 {
		return nil, true
	}
	resolved := make([]string, 0, len(backings))
	for _, backing := range backings {
		backing, ok := cleanSourceTreePath(backing)
		if !ok {
			return nil, false
		}
		if _, ok := s.absForTreePath(backing); !ok {
			return nil, false
		}
		resolved = appendUnique(resolved, backing)
	}
	return resolved, true
}

// refsForSourceDir returns the union of CONFIG_* footprints of every C and
// assembly source directly under dir. Special image objects compile multiple
// payload sources inside one action, so scanning only their nominal primary
// source would be incomplete.
func (s *configSourceScanner) refsForSourceDir(dir string) []string {
	if s == nil || s.sourceRoot == "" {
		return nil
	}
	base := filepath.Join(s.sourceRoot, filepath.FromSlash(dir))
	refset := map[string]bool{}
	_ = filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		switch filepath.Ext(entry.Name()) {
		case ".c", ".S", ".s":
			rel, relErr := filepath.Rel(s.sourceRoot, path)
			if relErr != nil {
				return nil
			}
			for _, ref := range s.refsForSource(filepath.ToSlash(rel), nil) {
				refset[ref] = true
			}
		}
		return nil
	})
	out := make([]string, 0, len(refset))
	for ref := range refset {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// scanFile returns the CONFIG_* tokens and literal and computed include
// targets of one file.
func (s *configSourceScanner) scanFile(abs string) (refs, rawIncludes, dynamicIncludes []string) {
	if cached, ok := s.fileRefs[abs]; ok {
		return cached, s.fileIncs[abs], s.fileDynamicIncs[abs]
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		s.fileRefs[abs] = nil
		s.fileIncs[abs] = nil
		s.fileDynamicIncs[abs] = []string{""}
		return nil, nil, []string{""}
	}
	text := string(data)
	refs = configRefs(text)
	for _, line := range preprocessorLines(text) {
		inc, literal, directive := includeDirective(line)
		if literal {
			rawIncludes = append(rawIncludes, inc)
		} else if directive {
			dynamicIncludes = append(dynamicIncludes, inc)
		}
		if assemblerIncbinDirective(line) {
			dynamicIncludes = append(dynamicIncludes, ".incbin")
		}
	}
	s.fileRefs[abs] = refs
	s.fileIncs[abs] = rawIncludes
	s.fileDynamicIncs[abs] = dynamicIncludes
	return refs, rawIncludes, dynamicIncludes
}

// resolveInclude returns every existing tree path an include could name.
// Returning all matches deliberately over-approximates the compiler's ordered
// search.
func (s *configSourceScanner) resolveInclude(fromTreePath, inc string, extraRoots []string) []string {
	inc = filepath.ToSlash(strings.TrimSpace(inc))
	if inc == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(treePath string) {
		treePath, ok := cleanSourceTreePath(treePath)
		if !ok || seen[treePath] {
			return
		}
		if _, ok := s.absForTreePath(treePath); !ok {
			return
		}
		seen[treePath] = true
		out = append(out, treePath)
	}
	if fromTreePath != "" {
		add(filepath.ToSlash(filepath.Join(filepath.Dir(fromTreePath), filepath.FromSlash(inc))))
	}
	for _, root := range s.includeRoots {
		add(root + "/" + inc)
	}
	for _, root := range extraRoots {
		if root == "" {
			add(inc)
		} else {
			add(root + "/" + inc)
		}
	}
	return out
}

func (s *configSourceScanner) absForTreePath(path string) (string, bool) {
	path, ok := cleanSourceTreePath(path)
	if !ok {
		return "", false
	}
	if s.sourceRoot != "" {
		abs := filepath.Join(s.sourceRoot, filepath.FromSlash(path))
		if fileExists(abs) {
			return abs, true
		}
	}
	if mapped, ok := mappedSourceRootPath(path, s.sourceRoots); ok && fileExists(mapped) {
		return mapped, true
	}
	return "", false
}

func cleanSourceTreePath(path string) (string, bool) {
	path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if path == "" || path == "." || path == ".." || strings.HasPrefix(path, "../") || filepath.IsAbs(filepath.FromSlash(path)) {
		return "", false
	}
	return path, true
}

// includeDirective distinguishes literal and computed C preprocessor includes.
func includeDirective(line string) (target string, literal, directive bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#") {
		return "", false, false
	}
	line = strings.TrimSpace(line[1:])
	if rest, ok := directiveKeywordRest(line, "include_next"); ok {
		return strings.TrimSpace(rest), false, true
	}
	rest, ok := directiveKeywordRest(line, "include")
	if !ok {
		return "", false, false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false, true
	}
	var closeByte byte
	switch rest[0] {
	case '"':
		closeByte = '"'
	case '<':
		closeByte = '>'
	default:
		return strings.TrimSpace(rest), false, true
	}
	rest = rest[1:]
	end := strings.IndexByte(rest, closeByte)
	if end < 0 {
		return "", false, true
	}
	return rest[:end], true, true
}

func assemblerIncbinDirective(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, ".incbin") {
		return false
	}
	rest := strings.TrimPrefix(line, ".incbin")
	return rest == "" || strings.ContainsAny(rest[:1], " \t\v\f\r\"")
}

func directiveKeywordRest(line, keyword string) (string, bool) {
	if !strings.HasPrefix(line, keyword) {
		return "", false
	}
	rest := line[len(keyword):]
	if rest == "" {
		return "", true
	}
	switch rest[0] {
	case ' ', '\t', '\v', '\f', '\r', '/', '\\', '"', '<':
		return rest, true
	default:
		return "", false
	}
}

func preprocessorLines(text string) []string {
	text = strings.ReplaceAll(text, "\\\r\n", "")
	text = strings.ReplaceAll(text, "\\\n", "")
	var out strings.Builder
	out.Grow(len(text))
	const (
		normal = iota
		lineComment
		blockComment
		stringLiteral
		charLiteral
	)
	state := normal
	for i := 0; i < len(text); i++ {
		current := text[i]
		next := byte(0)
		if i+1 < len(text) {
			next = text[i+1]
		}
		switch state {
		case normal:
			switch {
			case current == '/' && next == '/':
				out.WriteByte(' ')
				i++
				state = lineComment
			case current == '/' && next == '*':
				out.WriteByte(' ')
				i++
				state = blockComment
			case current == '"':
				out.WriteByte(current)
				state = stringLiteral
			case current == '\'':
				out.WriteByte(current)
				state = charLiteral
			default:
				out.WriteByte(current)
			}
		case lineComment:
			if current == '\n' {
				out.WriteByte(current)
				state = normal
			}
		case blockComment:
			if current == '*' && next == '/' {
				i++
				state = normal
			} else if current == '\n' {
				out.WriteByte(current)
			}
		case stringLiteral, charLiteral:
			out.WriteByte(current)
			if current == '\\' && i+1 < len(text) {
				i++
				out.WriteByte(text[i])
			} else if (state == stringLiteral && current == '"') || (state == charLiteral && current == '\'') {
				state = normal
			}
		}
	}
	return strings.Split(out.String(), "\n")
}
