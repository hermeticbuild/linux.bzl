// Package toolsetpath resolves stable, scope-qualified toolset paths through
// the exact typed Bazel artifacts supplied to one action.
package toolsetpath

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

const (
	HandoffEnvironmentName = "LINUX_BZL_TOOLSET_BINDINGS_V1"
	// ShellAliasRootPath is the deterministic, action-private spelling used in
	// evaluated shell source. Scriptrun installs Resolver's projection root as
	// its first ExtraFiles entry, which POSIX Go processes expose as child fd 3.
	ShellAliasRootPath = "/proc/self/fd/3"
	handoffSchema      = "linux-bzl-toolset-bindings-v1"
	maxHandoffBytes    = 64 << 20
)

var ErrPathOutsideClosure = errors.New("toolset path is not bound to this action")

type artifactBinding struct {
	canonical string
	path      string
	directory bool
}

type scopeResolver struct {
	scope          string
	identity       string
	exact          map[string]artifactBinding
	ordered        []artifactBinding
	projectionRoot string
	projections    map[string]string
	shellAliases   bool
}

// Resolver keeps host and target authority separate. A provenance token may
// resolve only through the identity-bound scope that created it.
type Resolver struct {
	byScope        map[string]*scopeResolver
	projectionRoot string
}

// LoadFlags consumes the scoped flags supplied by the Bazel action. Identity
// and manifest flags have SCOPE=VALUE syntax; anchors have SCOPE=ROOT=PATH
// syntax. The identity-bound manifest assigns every closure artifact to one
// root and names one typed anchor in that root. Bazel renders those few anchor
// Files with this consumer action's path mapper, so their physical paths prove
// the mapped root used by every sibling artifact without forwarding one argv
// entry per closure File.
func LoadFlags(execroot, projectionRoot string, identities, manifests, anchors []string) (*Resolver, error) {
	if execroot == "" || !filepath.IsAbs(execroot) {
		return nil, fmt.Errorf("toolset execution root must be an absolute path")
	}
	if projectionRoot == "" || !filepath.IsAbs(projectionRoot) {
		return nil, fmt.Errorf("toolset projection root must be an absolute path")
	}
	identityByScope, err := uniqueScopedValues("identity", identities)
	if err != nil {
		return nil, err
	}
	manifestByScope, err := uniqueScopedValues("manifest", manifests)
	if err != nil {
		return nil, err
	}
	anchorsByScope, err := scopedRootValues("anchor", anchors)
	if err != nil {
		return nil, err
	}
	if len(identityByScope) == 0 && len(manifestByScope) == 0 && len(anchorsByScope) == 0 {
		return nil, fmt.Errorf("toolset bindings contain no scopes")
	}

	scopes := map[string]bool{}
	for scope := range identityByScope {
		scopes[scope] = true
	}
	for scope := range manifestByScope {
		scopes[scope] = true
	}
	for scope := range anchorsByScope {
		scopes[scope] = true
	}
	projectionRoot = filepath.Clean(projectionRoot)
	resolver := &Resolver{byScope: map[string]*scopeResolver{}, projectionRoot: projectionRoot}
	for _, scope := range sortedScopeKeys(scopes) {
		identity := identityByScope[scope]
		manifestFilename := manifestByScope[scope]
		if identity == "" || manifestFilename == "" || len(anchorsByScope[scope]) == 0 {
			return nil, fmt.Errorf("toolset scope %s must have exactly one identity, one manifest, and one or more typed root anchors", scope)
		}
		scopeResolver, err := loadScope(
			filepath.Clean(execroot),
			filepath.Join(projectionRoot, scope),
			scope,
			identity,
			manifestFilename,
			anchorsByScope[scope],
		)
		if err != nil {
			return nil, err
		}
		resolver.byScope[scope] = scopeResolver
	}
	return resolver, nil
}

func uniqueScopedValues(kind string, values []string) (map[string]string, error) {
	out := map[string]string{}
	for _, value := range values {
		scope, item, err := splitScopedValue(kind, value)
		if err != nil {
			return nil, err
		}
		if _, exists := out[scope]; exists {
			return nil, fmt.Errorf("toolset repeats %s for scope %s", kind, scope)
		}
		out[scope] = item
	}
	return out, nil
}

func scopedRootValues(kind string, values []string) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, value := range values {
		scope, rest, ok := strings.Cut(value, "=")
		if !ok || rest == "" {
			return nil, fmt.Errorf("toolset %s %q must be SCOPE=ROOT=PATH", kind, value)
		}
		if scope != "host" && scope != "target" {
			return nil, fmt.Errorf("toolset %s %q has unknown scope %q", kind, value, scope)
		}
		root, filename, ok := strings.Cut(rest, "=")
		if !ok || root == "" || filename == "" || strings.ContainsAny(root, "/\\=\x00 \t\r\n") || strings.ContainsRune(filename, '\x00') {
			return nil, fmt.Errorf("toolset %s %q must be SCOPE=ROOT=PATH with a canonical root ID", kind, value)
		}
		if out[scope] == nil {
			out[scope] = map[string]string{}
		}
		if _, exists := out[scope][root]; exists {
			return nil, fmt.Errorf("toolset repeats %s root %s for scope %s", kind, root, scope)
		}
		out[scope][root] = filename
	}
	return out, nil
}

func splitScopedValue(kind, value string) (string, string, error) {
	scope, item, ok := strings.Cut(value, "=")
	if !ok || item == "" {
		return "", "", fmt.Errorf("toolset %s %q must be SCOPE=VALUE", kind, value)
	}
	if scope != "host" && scope != "target" {
		return "", "", fmt.Errorf("toolset %s %q has unknown scope %q", kind, value, scope)
	}
	if strings.ContainsRune(item, '\x00') {
		return "", "", fmt.Errorf("toolset %s for scope %s contains NUL", kind, scope)
	}
	return scope, item, nil
}

func loadScope(execroot, projectionRoot, scope, identity, manifestFilename string, anchors map[string]string) (*scopeResolver, error) {
	manifestPath, err := absoluteArtifactPath(execroot, manifestFilename)
	if err != nil {
		return nil, fmt.Errorf("%s toolset manifest: %w", scope, err)
	}
	manifest, err := toolaction.ReadKbuildToolsetManifest(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read %s toolset manifest: %w", scope, err)
	}
	if manifest.Scope != scope {
		return nil, fmt.Errorf("%s toolset manifest has scope %q", scope, manifest.Scope)
	}
	gotIdentity, err := manifest.Identity()
	if err != nil {
		return nil, fmt.Errorf("identify %s toolset manifest: %w", scope, err)
	}
	if gotIdentity != identity {
		return nil, fmt.Errorf("%s toolset manifest identity is %q, want %q", scope, gotIdentity, identity)
	}
	if len(anchors) != len(manifest.Roots) {
		return nil, fmt.Errorf("%s toolset has %d typed root anchors, manifest has %d roots", scope, len(anchors), len(manifest.Roots))
	}

	resolver := &scopeResolver{
		scope:          scope,
		identity:       identity,
		exact:          make(map[string]artifactBinding, len(manifest.Closure)),
		projectionRoot: projectionRoot,
		projections:    map[string]string{},
	}
	physicalRoots := make(map[string]string, len(manifest.Roots))
	for root, anchorCanonical := range manifest.Roots {
		filename, exists := anchors[root]
		if !exists {
			return nil, fmt.Errorf("%s toolset omits typed anchor for manifest root %q", scope, root)
		}
		physical, err := absoluteArtifactPath(execroot, filename)
		if err != nil {
			return nil, fmt.Errorf("%s toolset root %q anchor: %w", scope, root, err)
		}
		canonical, err := canonicalArtifactPath(execroot, physical)
		if err != nil {
			return nil, fmt.Errorf("canonicalize %s toolset root %q anchor %q: %w", scope, root, filename, err)
		}
		if canonical != anchorCanonical {
			return nil, fmt.Errorf("%s toolset root %q anchor resolves to %q, manifest binds %q", scope, root, canonical, anchorCanonical)
		}
		location := manifest.ArtifactRoots[anchorCanonical]
		if location.Root != root {
			return nil, fmt.Errorf("%s toolset root %q anchor %q belongs to root %q", scope, root, anchorCanonical, location.Root)
		}
		physicalRoot, err := artifactPhysicalRoot(physical, location.Path)
		if err != nil {
			return nil, fmt.Errorf("%s toolset root %q anchor %q: %w", scope, root, anchorCanonical, err)
		}
		physicalRoots[root] = physicalRoot
	}
	for root := range anchors {
		if _, exists := manifest.Roots[root]; !exists {
			return nil, fmt.Errorf("%s toolset has typed anchor for unknown manifest root %q", scope, root)
		}
	}

	for _, canonical := range manifest.Closure {
		location := manifest.ArtifactRoots[canonical]
		physicalRoot := physicalRoots[location.Root]
		if physicalRoot == "" {
			return nil, fmt.Errorf("%s toolset artifact %q has no physical root %q", scope, canonical, location.Root)
		}
		physical := filepath.Join(physicalRoot, filepath.FromSlash(location.Path))
		if !pathWithin(physicalRoot, physical) {
			return nil, fmt.Errorf("%s toolset artifact %q escapes physical root %q", scope, canonical, location.Root)
		}
		gotCanonical, err := canonicalArtifactPath(execroot, physical)
		if err != nil {
			return nil, fmt.Errorf("canonicalize %s toolset artifact %q: %w", scope, canonical, err)
		}
		if gotCanonical != canonical {
			return nil, fmt.Errorf("%s toolset artifact %q reconstructs as canonical path %q", scope, canonical, gotCanonical)
		}
		kind := manifest.ArtifactKinds[canonical]
		info, err := os.Stat(physical)
		if err != nil {
			return nil, fmt.Errorf("inspect %s toolset artifact %q: %w", scope, canonical, err)
		}
		directory := kind == toolaction.KbuildToolsetArtifactGeneratedDirectory
		if kind == toolaction.KbuildToolsetArtifactSource {
			// Bazel analysis cannot distinguish a legacy opaque source
			// directory from a source file. The exact identity-bound artifact is
			// safe to inspect here.
			directory = info.IsDir()
		} else if info.IsDir() != directory {
			return nil, fmt.Errorf("%s toolset artifact %q is %s on disk, manifest binds kind %q", scope, canonical, map[bool]string{true: "a directory", false: "not a directory"}[info.IsDir()], kind)
		}
		binding := artifactBinding{canonical: canonical, path: physical, directory: directory}
		resolver.exact[canonical] = binding
		resolver.ordered = append(resolver.ordered, binding)
	}
	sortBindingsLongestFirst(resolver.ordered)
	return resolver, nil
}

func artifactPhysicalRoot(anchor, relative string) (string, error) {
	if anchor == "" || !filepath.IsAbs(anchor) || filepath.Clean(anchor) != anchor {
		return "", fmt.Errorf("physical anchor path is not canonical and absolute")
	}
	if err := toolaction.ValidateCanonicalArtifactPath(relative); err != nil {
		return "", fmt.Errorf("invalid manifest-bound root-relative path %q: %w", relative, err)
	}
	root := anchor
	for range strings.Split(relative, "/") {
		root = filepath.Dir(root)
	}
	if filepath.Clean(filepath.Join(root, filepath.FromSlash(relative))) != anchor {
		return "", fmt.Errorf("physical anchor does not end in manifest-bound path %q", relative)
	}
	return root, nil
}

func absoluteArtifactPath(execroot, filename string) (string, error) {
	if filename == "" || strings.ContainsRune(filename, '\x00') {
		return "", fmt.Errorf("artifact path is empty or contains NUL")
	}
	if !filepath.IsAbs(filename) {
		filename = filepath.Join(execroot, filename)
	}
	return filepath.Clean(filename), nil
}

func canonicalArtifactPath(execroot, filename string) (string, error) {
	relative, err := filepath.Rel(filepath.Clean(execroot), filepath.Clean(filename))
	if err != nil {
		return "", err
	}
	return toolaction.CanonicalArtifactPath(filepath.ToSlash(relative))
}

// Resolve maps a canonical path through the exact closure of its scope.
func (r *Resolver) Resolve(scope, canonical string) (string, error) {
	resolver := r.byScope[scope]
	if resolver == nil {
		return "", fmt.Errorf("action has no typed %s toolset scope", scope)
	}
	return resolver.resolve(canonical)
}

// OpenShellAliasRoot opens the private projection root which scriptrun exposes
// to its child as fd 3. RewriteShell deliberately renders only the stable
// /proc/self/fd/3 spelling, never this process-private physical path.
func (r *Resolver) OpenShellAliasRoot() (*os.File, error) {
	if r == nil || r.projectionRoot == "" || !filepath.IsAbs(r.projectionRoot) {
		return nil, fmt.Errorf("toolset shell alias root is not canonical and absolute")
	}
	root, err := os.Open(r.projectionRoot)
	if err != nil {
		return nil, fmt.Errorf("open toolset shell alias root: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = root.Close()
		}
	}()
	info, err := root.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect toolset shell alias root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("toolset shell alias root is not a directory")
	}
	procPath := fmt.Sprintf("/proc/self/fd/%d", root.Fd())
	procInfo, err := os.Stat(procPath)
	if err != nil {
		return nil, fmt.Errorf("toolset shell aliases require procfs file-descriptor paths: %w", err)
	}
	if !os.SameFile(info, procInfo) {
		return nil, fmt.Errorf("toolset shell alias descriptor does not resolve to its backing root")
	}
	ok = true
	return root, nil
}

func (r *Resolver) resolveShellAlias(scope, canonical string) (string, error) {
	resolver := r.byScope[scope]
	if resolver == nil {
		return "", fmt.Errorf("action has no typed %s toolset scope", scope)
	}
	if err := validateShellAliasCanonicalPath(canonical); err != nil {
		return "", fmt.Errorf("invalid canonical %s toolset shell path %q: %w", scope, canonical, err)
	}
	// Authorize the exact path (or a descendant/ancestor projection) against the
	// identity-bound closure before emitting any visible alias.
	if _, err := resolver.resolve(canonical); err != nil {
		return "", err
	}
	if err := r.materializeShellAliases(resolver); err != nil {
		return "", err
	}
	return path.Join(ShellAliasRootPath, "aliases", scope, resolver.identity, canonical), nil
}

func (r *Resolver) materializeShellAliases(resolver *scopeResolver) error {
	if resolver.shellAliases {
		return nil
	}
	selected := selectTopLevelBindings(resolver.ordered)
	identityRoot := filepath.Join(r.projectionRoot, "aliases", resolver.scope, resolver.identity)
	if !pathWithin(r.projectionRoot, identityRoot) {
		return fmt.Errorf("canonical %s toolset shell alias root escapes its private projection", resolver.scope)
	}
	if err := os.MkdirAll(identityRoot, 0o700); err != nil {
		return fmt.Errorf("create canonical %s toolset shell alias root: %w", resolver.scope, err)
	}
	for _, binding := range selected {
		destination := filepath.Join(identityRoot, filepath.FromSlash(binding.canonical))
		if !pathWithin(identityRoot, destination) {
			return fmt.Errorf("%s toolset artifact %q escapes its shell alias root", resolver.scope, binding.canonical)
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return fmt.Errorf("create canonical %s toolset shell alias parent for %q: %w", resolver.scope, binding.canonical, err)
		}
		physical, err := filepath.EvalSymlinks(binding.path)
		if err != nil {
			return fmt.Errorf("resolve canonical %s toolset shell artifact %q: %w", resolver.scope, binding.canonical, err)
		}
		if err := materializeProjectionSymlink(destination, physical); err != nil {
			return fmt.Errorf("project canonical %s toolset shell artifact %q: %w", resolver.scope, binding.canonical, err)
		}
	}
	resolver.shellAliases = true
	return nil
}

func validateShellAliasCanonicalPath(canonical string) error {
	if err := toolaction.ValidateCanonicalArtifactPath(canonical); err != nil {
		return err
	}
	for _, component := range strings.Split(canonical, "/") {
		for index := 0; index < len(component); index++ {
			character := component[index]
			if (character >= 'a' && character <= 'z') ||
				(character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') ||
				strings.ContainsRune("._-+", rune(character)) {
				continue
			}
			return fmt.Errorf("path component %q contains byte %#x outside the conservative shell alias alphabet", component, character)
		}
	}
	return nil
}

// Rewrite validates and resolves every private provenance token. A nil
// resolver is allowed only when value contains no token, which lets pure copy
// recipes avoid carrying an otherwise unused compiler closure while still
// failing closed if one appears.
func Rewrite(value string, resolver *Resolver) (string, error) {
	return rewrite(value, resolver, func(path string) string { return path })
}

// RewriteShell validates and resolves every private provenance token embedded
// in POSIX shell source. Each physical path is escaped for the token's lexical
// quote context. This matters for ordinary Kbuild compile recipes: the command
// is executed once and also passed as one single-quoted argument to fixdep.
func RewriteShell(value string, resolver *Resolver) (string, error) {
	contexts, err := shellProvenanceTokenContexts(value)
	if err != nil {
		return "", err
	}
	ordinal := 0
	rewritten, err := rewriteResolved(value, func(scope, canonical string) (string, error) {
		if resolver == nil {
			return "", fmt.Errorf("action has no bound toolset scopes")
		}
		return resolver.resolveShellAlias(scope, canonical)
	}, func(path string) string {
		if ordinal >= len(contexts) {
			return ""
		}
		context := contexts[ordinal]
		ordinal++
		switch context {
		case shellQuoteSingle:
			return quotePOSIXShellSingleQuotedFragment(path)
		case shellQuoteDouble:
			return quotePOSIXShellDoubleQuotedFragment(path)
		default:
			return quotePOSIXShellWord(path)
		}
	})
	if err != nil {
		return "", err
	}
	if ordinal != len(contexts) {
		return "", fmt.Errorf("toolset-path shell context count changed during rewrite")
	}
	return rewritten, nil
}

func rewrite(value string, resolver *Resolver, render func(string) string) (string, error) {
	return rewriteResolved(value, func(scope, canonical string) (string, error) {
		if resolver == nil {
			return "", fmt.Errorf("action has no bound toolset scopes")
		}
		return resolver.Resolve(scope, canonical)
	}, render)
}

func rewriteResolved(
	value string,
	resolve func(scope, canonical string) (string, error),
	render func(string) string,
) (string, error) {
	return toolaction.RewriteExecutionRootProvenanceValue(value, func(scope, canonical string) (string, error) {
		resolved, err := resolve(scope, canonical)
		if err != nil {
			return "", err
		}
		return render(resolved), nil
	})
}

func quotePOSIXShellWord(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func quotePOSIXShellSingleQuotedFragment(value string) string {
	return strings.ReplaceAll(value, "'", "'\"'\"'")
}

func quotePOSIXShellDoubleQuotedFragment(value string) string {
	return strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"$", "\\$",
		"`", "\\`",
	).Replace(value)
}

const (
	shellQuoteNone   byte = 0
	shellQuoteSingle byte = '\''
	shellQuoteDouble byte = '"'
)

// shellProvenanceTokenContexts identifies the lexical quote context of every
// token. Reconstructing a complete POSIX shell parser in the runner would be
// brittle, so provenance-bearing scripts fail closed around contexts such as
// comments, parameter expansion, and heredocs which are not ordinary shell
// word fragments. Those constructs may still appear in token-free scripts.
func shellProvenanceTokenContexts(value string) ([]byte, error) {
	if !strings.ContainsAny(value, "\x07\x08") {
		return nil, nil
	}
	if err := toolaction.ValidateExecutionRootProvenanceValue(value); err != nil {
		return nil, err
	}
	// POSIX removes an unquoted backslash-newline before recognizing shell
	// operators. Scan that logical source so a split spelling such as
	// <\\\n< cannot hide a heredoc from the context checks below. Removing the
	// same pair inside single quotes is harmless for this projection: it changes
	// literal bytes, but cannot change whether a later provenance token is
	// quoted. Preserve escaped backslashes so only an odd final backslash joins
	// two physical lines.
	value = projectShellLineContinuations(value)
	quote := shellQuoteNone
	escaped := false
	comment := false
	parameterQuotes := []byte(nil)
	atWordStart := true
	contexts := make([]byte, 0, strings.Count(value, toolaction.ExecutionRootProvenanceMarker))
	for index := 0; index < len(value); {
		if strings.HasPrefix(value[index:], toolaction.ExecutionRootProvenanceMarker) {
			switch {
			case comment:
				return nil, fmt.Errorf("toolset path token at byte %d appears in a shell comment", index)
			case escaped:
				return nil, fmt.Errorf("toolset path token at byte %d is shell-escaped", index)
			case len(parameterQuotes) != 0:
				return nil, fmt.Errorf("toolset path token at byte %d appears in shell parameter expansion", index)
			}
			contexts = append(contexts, quote)
			index = shellProvenanceTokenEnvelopeEnd(value, index)
			if index < len(value) && !isPOSIXShellTokenBoundary(value[index]) {
				return nil, fmt.Errorf("toolset path token at byte %d is not followed by a POSIX shell word boundary", index)
			}
			atWordStart = false
			continue
		}
		character := value[index]
		if comment {
			if character == '\n' {
				comment = false
				atWordStart = true
			}
			index++
			continue
		}
		if escaped {
			escaped = false
			atWordStart = false
			index++
			continue
		}
		if quote == shellQuoteSingle {
			if character == shellQuoteSingle {
				quote = shellQuoteNone
			}
			index++
			continue
		}
		if character == '\\' {
			escaped = true
			atWordStart = false
			index++
			continue
		}
		if character == '`' || strings.HasPrefix(value[index:], "$(") ||
			(quote == shellQuoteNone && (strings.HasPrefix(value[index:], "<(") || strings.HasPrefix(value[index:], ">("))) {
			return nil, fmt.Errorf("toolset-path shell replay does not accept command or process substitution")
		}
		// Parameter expansion can begin in ordinary or double-quoted text. Record
		// that quote context so a brace in a nested quoted fragment cannot close
		// the expansion early and make a later token appear authoritative.
		if character == '$' && strings.HasPrefix(value[index:], "${") {
			parameterQuotes = append(parameterQuotes, quote)
			atWordStart = false
			index += 2
			continue
		}
		if character == '}' && len(parameterQuotes) != 0 && quote == parameterQuotes[len(parameterQuotes)-1] {
			parameterQuotes = parameterQuotes[:len(parameterQuotes)-1]
			atWordStart = false
			index++
			continue
		}
		if quote == shellQuoteDouble {
			if character == shellQuoteDouble {
				quote = shellQuoteNone
			}
			index++
			continue
		}
		switch character {
		case shellQuoteSingle, shellQuoteDouble:
			quote = character
			atWordStart = false
			index++
		case '#':
			if len(parameterQuotes) == 0 && atWordStart {
				comment = true
			} else {
				atWordStart = false
			}
			index++
		case '$':
			index++
			atWordStart = false
		case '}':
			atWordStart = false
			index++
		case '<':
			if strings.HasPrefix(value[index:], "<<") {
				return nil, fmt.Errorf("toolset-path shell replay does not accept heredoc or here-string syntax")
			}
			if len(parameterQuotes) == 0 {
				atWordStart = true
			}
			index++
		case ' ', '\t', '\n':
			if len(parameterQuotes) == 0 {
				atWordStart = true
			}
			index++
		case ';', '&', '|', '(', ')', '>':
			if len(parameterQuotes) == 0 {
				atWordStart = true
			}
			index++
		default:
			atWordStart = false
			index++
		}
	}
	if quote != shellQuoteNone || escaped || len(parameterQuotes) != 0 {
		return nil, fmt.Errorf("toolset-path shell replay cannot establish a complete shell context")
	}
	return contexts, nil
}

func isPOSIXShellTokenBoundary(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n'
}

func projectShellLineContinuations(value string) string {
	if !strings.Contains(value, "\\\n") {
		return value
	}
	var projected strings.Builder
	projected.Grow(len(value))
	for index := 0; index < len(value); {
		if value[index] != '\\' {
			projected.WriteByte(value[index])
			index++
			continue
		}
		start := index
		for index < len(value) && value[index] == '\\' {
			index++
		}
		if index < len(value) && value[index] == '\n' && (index-start)%2 != 0 {
			projected.WriteString(value[start : index-1])
			index++
			continue
		}
		projected.WriteString(value[start:index])
	}
	return projected.String()
}

func shellProvenanceTokenEnvelopeEnd(value string, start int) int {
	terminator := strings.Index(value[start+len(toolaction.ExecutionRootProvenanceMarker):], toolaction.ExecutionRootProvenanceTerminator)
	if terminator < 0 {
		return len(value)
	}
	end := start + len(toolaction.ExecutionRootProvenanceMarker) + terminator + len(toolaction.ExecutionRootProvenanceTerminator)
	for end < len(value) {
		switch value[end] {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			return end
		default:
			end++
		}
	}
	return end
}

func (r *scopeResolver) resolve(canonical string) (string, error) {
	if err := toolaction.ValidateCanonicalArtifactPath(canonical); err != nil {
		return "", fmt.Errorf("invalid canonical %s toolset path %q: %w", r.scope, canonical, err)
	}
	if binding, exists := r.exact[canonical]; exists {
		return binding.path, nil
	}
	for _, binding := range r.ordered {
		if !binding.directory || !strings.HasPrefix(canonical, binding.canonical+"/") {
			continue
		}
		relative := strings.TrimPrefix(canonical, binding.canonical+"/")
		candidate := filepath.Join(binding.path, filepath.FromSlash(relative))
		if err := ensurePathInsideDirectory(binding.path, candidate); err != nil {
			return "", err
		}
		return candidate, nil
	}
	return r.projectCanonicalDirectory(canonical)
}

func (r *scopeResolver) projectCanonicalDirectory(canonical string) (string, error) {
	if projection := r.projections[canonical]; projection != "" {
		return projection, nil
	}
	prefix := canonical + "/"
	bindings := make([]artifactBinding, 0)
	for _, binding := range r.ordered {
		if strings.HasPrefix(binding.canonical, prefix) {
			bindings = append(bindings, binding)
		}
	}
	if len(bindings) == 0 {
		return "", fmt.Errorf("canonical path %q is outside the identity-bound %s toolset closure: %w", canonical, r.scope, ErrPathOutsideClosure)
	}
	selected := selectTopLevelBindings(bindings)

	projection := filepath.Join(r.projectionRoot, filepath.FromSlash(canonical))
	if !pathWithin(r.projectionRoot, projection) {
		return "", fmt.Errorf("canonical %s toolset projection %q escapes its private root", r.scope, canonical)
	}
	if err := os.MkdirAll(projection, 0o700); err != nil {
		return "", fmt.Errorf("create canonical %s toolset projection %q: %w", r.scope, canonical, err)
	}
	for _, binding := range selected {
		suffix := strings.TrimPrefix(binding.canonical, prefix)
		destination := filepath.Join(projection, filepath.FromSlash(suffix))
		if !pathWithin(projection, destination) {
			return "", fmt.Errorf("%s toolset artifact %q escapes canonical projection %q", r.scope, binding.canonical, canonical)
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return "", fmt.Errorf("create canonical %s toolset projection parent for %q: %w", r.scope, binding.canonical, err)
		}
		physical, err := filepath.EvalSymlinks(binding.path)
		if err != nil {
			return "", fmt.Errorf("resolve canonical %s toolset artifact %q: %w", r.scope, binding.canonical, err)
		}
		if err := materializeProjectionSymlink(destination, physical); err != nil {
			return "", fmt.Errorf("project canonical %s toolset artifact %q: %w", r.scope, binding.canonical, err)
		}
	}
	r.projections[canonical] = projection
	return projection, nil
}

// selectTopLevelBindings preserves the declared closure hierarchy without
// shadowing descendants already reachable through a declared directory.
func selectTopLevelBindings(bindings []artifactBinding) []artifactBinding {
	ordered := append([]artifactBinding(nil), bindings...)
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i].canonical) != len(ordered[j].canonical) {
			return len(ordered[i].canonical) < len(ordered[j].canonical)
		}
		return ordered[i].canonical < ordered[j].canonical
	})
	selected := make([]artifactBinding, 0, len(ordered))
	for _, binding := range ordered {
		covered := false
		for _, prior := range selected {
			if prior.directory && strings.HasPrefix(binding.canonical, prior.canonical+"/") {
				covered = true
				break
			}
		}
		if !covered {
			selected = append(selected, binding)
		}
	}
	return selected
}

func materializeProjectionSymlink(destination, physical string) error {
	if err := os.Symlink(physical, destination); err == nil {
		return nil
	} else if !os.IsExist(err) {
		return err
	}
	existing, err := filepath.EvalSymlinks(destination)
	if err != nil {
		return fmt.Errorf("inspect existing projection path: %w", err)
	}
	if filepath.Clean(existing) != filepath.Clean(physical) {
		return fmt.Errorf("projection path already resolves to %q, want %q", existing, physical)
	}
	return nil
}

func ensurePathInsideDirectory(root, candidate string) error {
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve typed toolset directory %q: %w", root, err)
	}
	candidateResolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return fmt.Errorf("resolve toolset path %q: %w", candidate, err)
	}
	if !pathWithin(rootResolved, candidateResolved) {
		return fmt.Errorf("toolset path %q escapes typed directory %q", candidate, root)
	}
	return nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

type handoffDocument struct {
	Schema string         `json:"schema"`
	Scopes []handoffScope `json:"scopes"`
}

type handoffScope struct {
	Scope     string            `json:"scope"`
	Identity  string            `json:"identity"`
	Artifacts []handoffArtifact `json:"artifacts"`
}

type handoffArtifact struct {
	Canonical string `json:"canonical"`
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
}

// CreateHandoff writes a private, self-contained canonical-to-physical table
// for a nested runner in the same Bazel action. The parent has already proved
// plan identity, manifest membership, artifact kind, and typed physical path.
func (r *Resolver) CreateHandoff(directory string) (string, error) {
	document := handoffDocument{Schema: handoffSchema}
	for _, scope := range sortedResolverScopes(r.byScope) {
		resolver := r.byScope[scope]
		entry := handoffScope{Scope: scope, Identity: resolver.identity}
		bindings := append([]artifactBinding(nil), resolver.ordered...)
		sort.Slice(bindings, func(i, j int) bool { return bindings[i].canonical < bindings[j].canonical })
		for _, binding := range bindings {
			entry.Artifacts = append(entry.Artifacts, handoffArtifact{
				Canonical: binding.canonical,
				Path:      binding.path,
				Directory: binding.directory,
			})
		}
		document.Scopes = append(document.Scopes, entry)
	}
	data, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode toolset binding handoff: %w", err)
	}
	file, err := os.CreateTemp(directory, "linux-bzl-toolset-bindings-*.json")
	if err != nil {
		return "", fmt.Errorf("create toolset binding handoff: %w", err)
	}
	filename := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(filename)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", fmt.Errorf("protect toolset binding handoff: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return "", fmt.Errorf("write toolset binding handoff: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close toolset binding handoff: %w", err)
	}
	ok = true
	return filename, nil
}

// LoadHandoff validates the private mapping passed to a nested runner and
// assigns it a fresh projection root. No raw execution-root authority crosses
// the process boundary.
func LoadHandoff(filename, projectionRoot string) (*Resolver, error) {
	if filename == "" {
		return nil, fmt.Errorf("toolset binding handoff path is empty")
	}
	if projectionRoot == "" || !filepath.IsAbs(projectionRoot) {
		return nil, fmt.Errorf("toolset projection root must be an absolute path")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open toolset binding handoff: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat toolset binding handoff: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxHandoffBytes {
		return nil, fmt.Errorf("toolset binding handoff is not a regular file within %d bytes", maxHandoffBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxHandoffBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read toolset binding handoff: %w", err)
	}
	if len(data) > maxHandoffBytes {
		return nil, fmt.Errorf("toolset binding handoff exceeds %d bytes", maxHandoffBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document handoffDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode toolset binding handoff: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("decode toolset binding handoff trailer: %w", err)
	}
	if document.Schema != handoffSchema || len(document.Scopes) == 0 {
		return nil, fmt.Errorf("toolset binding handoff has invalid schema or no scopes")
	}

	projectionRoot = filepath.Clean(projectionRoot)
	resolver := &Resolver{byScope: map[string]*scopeResolver{}, projectionRoot: projectionRoot}
	previousScope := ""
	for _, scope := range document.Scopes {
		if scope.Scope != "host" && scope.Scope != "target" {
			return nil, fmt.Errorf("toolset binding handoff has unknown scope %q", scope.Scope)
		}
		if previousScope != "" && scope.Scope <= previousScope {
			return nil, fmt.Errorf("toolset binding handoff scopes are not strictly sorted")
		}
		previousScope = scope.Scope
		if len(scope.Artifacts) == 0 {
			return nil, fmt.Errorf("toolset binding handoff scope %s has no artifacts", scope.Scope)
		}
		if !validIdentity(scope.Identity) {
			return nil, fmt.Errorf("toolset binding handoff scope %s has invalid identity", scope.Scope)
		}
		scopeResolver := &scopeResolver{
			scope:          scope.Scope,
			identity:       scope.Identity,
			exact:          map[string]artifactBinding{},
			projectionRoot: filepath.Join(projectionRoot, scope.Scope),
			projections:    map[string]string{},
		}
		previousCanonical := ""
		for _, artifact := range scope.Artifacts {
			if err := toolaction.ValidateCanonicalArtifactPath(artifact.Canonical); err != nil {
				return nil, fmt.Errorf("toolset binding handoff %s path %q: %w", scope.Scope, artifact.Canonical, err)
			}
			if previousCanonical != "" && artifact.Canonical <= previousCanonical {
				return nil, fmt.Errorf("toolset binding handoff %s artifacts are not strictly sorted", scope.Scope)
			}
			previousCanonical = artifact.Canonical
			if artifact.Path == "" || !filepath.IsAbs(artifact.Path) || filepath.Clean(artifact.Path) != artifact.Path || strings.ContainsRune(artifact.Path, '\x00') {
				return nil, fmt.Errorf("toolset binding handoff %s artifact %q has invalid physical path", scope.Scope, artifact.Canonical)
			}
			info, err := os.Stat(artifact.Path)
			if err != nil {
				return nil, fmt.Errorf("inspect toolset binding handoff %s artifact %q: %w", scope.Scope, artifact.Canonical, err)
			}
			if info.IsDir() != artifact.Directory {
				return nil, fmt.Errorf("toolset binding handoff %s artifact %q changed kind", scope.Scope, artifact.Canonical)
			}
			binding := artifactBinding{canonical: artifact.Canonical, path: artifact.Path, directory: artifact.Directory}
			scopeResolver.exact[artifact.Canonical] = binding
			scopeResolver.ordered = append(scopeResolver.ordered, binding)
		}
		sortBindingsLongestFirst(scopeResolver.ordered)
		resolver.byScope[scope.Scope] = scopeResolver
	}
	return resolver, nil
}

func sortBindingsLongestFirst(bindings []artifactBinding) {
	sort.Slice(bindings, func(i, j int) bool {
		left, right := bindings[i].canonical, bindings[j].canonical
		if len(left) != len(right) {
			return len(left) > len(right)
		}
		return left < right
	})
}

func sortedScopeKeys(scopes map[string]bool) []string {
	keys := make([]string, 0, len(scopes))
	for scope := range scopes {
		keys = append(keys, scope)
	}
	sort.Strings(keys)
	return keys
}

func sortedResolverScopes(scopes map[string]*scopeResolver) []string {
	keys := make([]string, 0, len(scopes))
	for scope := range scopes {
		keys = append(keys, scope)
	}
	sort.Strings(keys)
	return keys
}

func validIdentity(value string) bool {
	if len(value) != len("sha256-")+64 || !strings.HasPrefix(value, "sha256-") {
		return false
	}
	digest := value[len("sha256-"):]
	_, err := hex.DecodeString(digest)
	return err == nil && strings.ToLower(digest) == digest
}
