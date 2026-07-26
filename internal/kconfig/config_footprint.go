package kconfig

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

	fileRefs map[string][]string
	fileIncs map[string][]string
	closure  map[string]sourceClosure
}

type sourceClosure struct {
	refs           []string
	sourceIncludes []string
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
		sourceRoot:   opts.SourceRoot,
		sourceRoots:  opts.SourceRoots,
		includeRoots: roots,
		fileRefs:     map[string][]string{},
		fileIncs:     map[string][]string{},
		closure:      map[string]sourceClosure{},
	}
}

// refsForSource returns the sorted CONFIG_* symbols reachable from source.
// extraIncludeRoots contains tree-relative directories extracted from the
// object's Kbuild -I flags.
func (s *configSourceScanner) refsForSource(source string, extraIncludeRoots []string) []string {
	return append([]string(nil), s.closureForSource(source, extraIncludeRoots).refs...)
}

// sourceIncludesForSource returns sorted source-like tree paths reached through
// literal includes. It follows the full include graph so source fragments
// included by a header are also explicit compile-action inputs.
func (s *configSourceScanner) sourceIncludesForSource(source string, extraIncludeRoots []string) []string {
	return append([]string(nil), s.closureForSource(source, extraIncludeRoots).sourceIncludes...)
}

func (s *configSourceScanner) closureForSource(source string, extraIncludeRoots []string) sourceClosure {
	if s == nil || source == "" {
		return sourceClosure{}
	}
	key := source + "\x00" + strings.Join(extraIncludeRoots, "\x00")
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
	stack := []string{source}
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
		refs, rawIncludes := s.scanFile(abs)
		for _, ref := range refs {
			refset[ref] = true
		}
		for _, inc := range rawIncludes {
			for _, resolved := range s.resolveInclude(treePath, inc, extraIncludeRoots) {
				if resolved != source && isSourceLikeInclude(resolved) {
					includeSet[resolved] = true
				}
				if !visited[resolved] {
					stack = append(stack, resolved)
				}
			}
		}
	}
	out := slices.Sorted(maps.Keys(refset))
	includes := slices.Sorted(maps.Keys(includeSet))
	result := sourceClosure{
		refs:           out,
		sourceIncludes: includes,
	}
	s.closure[key] = result
	return result
}

func isSourceLikeInclude(path string) bool {
	return strings.HasSuffix(path, ".c") ||
		strings.HasSuffix(path, ".S") ||
		strings.HasSuffix(path, ".s") ||
		strings.HasSuffix(path, ".inc")
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
	return slices.Sorted(maps.Keys(refset))
}

// scanFile returns the CONFIG_* tokens and raw include targets of one file.
func (s *configSourceScanner) scanFile(abs string) (refs, rawIncludes []string) {
	if cached, ok := s.fileRefs[abs]; ok {
		return cached, s.fileIncs[abs]
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		s.fileRefs[abs] = nil
		s.fileIncs[abs] = nil
		return nil, nil
	}
	text := string(data)
	refs = configRefs(text)
	for _, line := range strings.Split(text, "\n") {
		if inc, ok := includePath(line); ok {
			rawIncludes = append(rawIncludes, inc)
		}
	}
	s.fileRefs[abs] = refs
	s.fileIncs[abs] = rawIncludes
	return refs, rawIncludes
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
	for _, candidate := range includeCandidates(inc) {
		for _, root := range s.includeRoots {
			add(root + "/" + candidate)
		}
		for _, root := range extraRoots {
			if root == "" {
				add(candidate)
			} else {
				add(root + "/" + candidate)
			}
		}
	}
	return out
}

// includeCandidates maps generated per-arch asm wrappers to their source-tree
// asm-generic counterparts as an additional conservative scan target.
func includeCandidates(inc string) []string {
	inc = filepath.ToSlash(inc)
	candidates := []string{inc}
	if rest, ok := strings.CutPrefix(inc, "asm/"); ok {
		candidates = append(candidates, "asm-generic/"+rest)
	} else if rest, ok := strings.CutPrefix(inc, "uapi/asm/"); ok {
		candidates = append(candidates, "uapi/asm-generic/"+rest)
	}
	return candidates
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

// includePath extracts a literal quoted or angled C preprocessor include.
func includePath(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#") {
		return "", false
	}
	line = strings.TrimSpace(line[1:])
	if !strings.HasPrefix(line, "include") {
		return "", false
	}
	rest := line[len("include"):]
	if rest == "" {
		return "", false
	}
	if c := rest[0]; c != '"' && c != '<' && c != ' ' && c != '\t' {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	var closeByte byte
	switch rest[0] {
	case '"':
		closeByte = '"'
	case '<':
		closeByte = '>'
	default:
		return "", false
	}
	rest = rest[1:]
	end := strings.IndexByte(rest, closeByte)
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}
